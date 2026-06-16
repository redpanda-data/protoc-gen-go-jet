// Command protoc-gen-go-jet is the go-jet toolkit for storage-backed
// proto resources. Three modes:
//
//	(no args)             — protoc plugin: read CodeGeneratorRequest
//	                        on stdin; emit ddl.sql + <resource>_mapper.go
//	                        + <resource>_schema.go + <resource>_aliases.go
//	                        per annotated message; then boot an ephemeral
//	                        Postgres, apply the emitted ddl.sql, and run
//	                        jet's template generator to emit the
//	                        go-jet model/table packages under
//	                        <out_dir>/jet/public/{model,table}.
//	from-db <dsn> <out>   — escape hatch: introspect an existing DSN
//	                        instead of materializing from ddl.sql. Rare.
//	check                 — verify hand-authored migrations produce the
//	                        same schema as ddl.sql (for every plugin-
//	                        managed table).
//	--version / --help    — diagnostics.
//
// Wire the plugin mode via buf:
//
//	plugins:
//	  - local: protoc-gen-go-jet
//	    out: .
//	    opt:
//	      - paths=source_relative
//	      - repo_root=.
//
// One `buf generate` invocation produces the complete storage layer —
// proto mappers, pagination schema, ddl.sql, and go-jet model/table
// packages. No follow-up task is needed. Docker is required (the plugin
// boots an ephemeral Postgres to drive jet's schema introspection).
//
// See the project README for the design.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-jet/jet/v2/generator/template"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

// version is stamped via -ldflags at build time. Left empty in dev builds
// so a mismatch between committed generated output and the dev plugin
// binary doesn't masquerade as a tagged release.
var version = "(devel)"

const help = `protoc-gen-go-jet — go-jet toolkit for proto-annotated resources.

Protoc plugin mode (default, via buf):

  plugins:
    - local: protoc-gen-go-jet
      out: .
      opt:
        - paths=source_relative
        - repo_root=.

One invocation emits every storage artifact for every annotated message:
ddl.sql, proto<->model mapper, aipjet schema, aliases, and the go-jet
model/table packages. Docker is required (an ephemeral Postgres is used
to drive jet's schema introspection).

Subcommands:

  from-db <dsn> <out_dir>
      Introspect an existing DSN. Escape hatch for environments where
      you cannot regenerate from proto — point it at a live DB.

  check --ddl-roots=<r1,r2> --migrations=<dir> [--tenant-role=<role>]
      Boot two PG containers; apply ddl.sql files to one and migrations
      to the other; assert structural equality per plugin-managed table.
      Exits 0 on match, non-zero with a path-qualified SQL diff on drift.

  diff --ddl-roots=<r1,r2> --migrations=<dir> [--tenant-role=<role>]
      Same setup as check, but instead of failing on drift, prints a
      SQL scaffold to stdout — ALTER / CREATE / DROP statements that
      turn the migrations-side schema into the ddl.sql-side schema.
      Redirect to NNNN_<desc>.up.sql and review before applying.

  --version / --help
      Diagnostics.

See cmd/protoc-gen-go-jet/README.md for the full options reference.`

func main() {
	// Subcommand dispatch. protoc / buf invoke the plugin with no
	// positional args (CodeGeneratorRequest arrives on stdin), so any
	// bare word in os.Args[1] is a subcommand. Dash-prefixed args are
	// either long flags we handle here or protogen parameters handled
	// by protogen.Options.
	if len(os.Args) >= 2 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "from-db":
			if len(os.Args) != 4 {
				slog.Error("usage: protoc-gen-go-jet from-db <dsn> <out_dir>")
				os.Exit(1)
			}
			if err := runFromDB(os.Args[2], os.Args[3], nil); err != nil {
				slog.Error("generation failed", slog.Any("err", err))
				os.Exit(1)
			}
			slog.Info("generation finished", slog.String("output", os.Args[3]))
			return
		case "check":
			if err := runCheck(os.Args[2:]); err != nil {
				slog.Error("drift check failed", slog.Any("err", err))
				os.Exit(1)
			}
			return
		case "diff":
			if err := runDiff(os.Args[2:]); err != nil {
				slog.Error("diff failed", slog.Any("err", err))
				os.Exit(1)
			}
			return
		default:
			fmt.Fprintf(os.Stderr, "protoc-gen-go-jet: unknown subcommand %q\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "Run `protoc-gen-go-jet --help` for usage.")
			os.Exit(2)
		}
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Println("protoc-gen-go-jet", version)
			return
		case "--help", "-h":
			fmt.Println(help)
			return
		}
	}

	var flags flag.FlagSet
	var repoRoot string
	flags.StringVar(&repoRoot, "repo_root", "", "absolute path of the repo root (where migrations and storage packages land). Required.")

	protogen.Options{
		ParamFunc: flags.Set,
	}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

		if repoRoot == "" {
			return errors.New("protoc-gen-go-jet: -repo_root is required")
		}

		// The consumer's module path is the prefix the plugin strips from
		// each proto package's Go import path to find its repo-relative
		// output directory. Reading it from go.mod (rather than hardcoding)
		// is what lets the plugin generate into any module.
		modulePath, err := readGoModulePath(repoRoot)
		if err != nil {
			return fmt.Errorf("protoc-gen-go-jet: %w", err)
		}

		// Two-pass: resolve every annotated message first, accumulate
		// any configuration errors, then emit. This keeps the plugin
		// atomic — a bad annotation on message N doesn't leave messages
		// 1..N-1 half-written to disk.
		var pending []pendingEmit
		var resolveErrs []error

		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			// Nested messages can't be persisted as standalone
			// resources today — the top-level iteration below only
			// walks f.Messages. Detect `(storage.v1.table)` on a
			// nested message and fail loudly rather than silently
			// ignoring the annotation. If we ever want to support
			// nested persisted resources, this is also where that
			// feature would plug in.
			for _, m := range f.Messages {
				if err := rejectNestedTableAnnotations(m); err != nil {
					resolveErrs = append(resolveErrs, err)
				}
			}
			for _, m := range f.Messages {
				plan, err := resolveResource(f, m, repoRoot, modulePath)
				if err != nil {
					resolveErrs = append(resolveErrs, fmt.Errorf("%s: %s: %w", sourceLoc(m.Desc), m.Desc.Name(), err))
					continue
				}
				if plan == nil {
					continue
				}
				pending = append(pending, pendingEmit{
					source: fmt.Sprintf("%s: %s", sourceLoc(m.Desc), m.Desc.Name()),
					plan:   plan,
				})
			}
		}
		if len(resolveErrs) > 0 {
			return joinErrs("protoc-gen-go-jet: resolution failed", resolveErrs)
		}

		// Cross-resource: two messages resolving to the same table
		// within the same Go output directory would both CREATE TABLE
		// the same name. The second one's DDL loses at apply, and the
		// error surfaces as a cryptic "relation already exists" from
		// drift-check rather than a descriptive annotation error.
		// Catch at resolve time instead.
		if errs := detectDuplicateTables(pending); len(errs) > 0 {
			return joinErrs("protoc-gen-go-jet: resolution failed", errs)
		}

		// Foreign-key resolution is a second cross-plan pass. It needs
		// the full registry to look targets up across proto files, so
		// it can't happen inside resolveResource which only sees one
		// message at a time. Running it here — after duplicate-table
		// detection — keeps the pass order: local syntax, cross-
		// message structure, cross-message references.
		plans := make([]*ResourcePlan, len(pending))
		for i, pe := range pending {
			plans[i] = pe.plan
		}
		if err := resolveForeignKeys(plans); err != nil {
			return err
		}

		var emitErrs []error
		for _, pe := range pending {
			if err := emit(gen, pe.plan); err != nil {
				emitErrs = append(emitErrs, fmt.Errorf("%s: emit: %w", pe.source, err))
			}
		}
		if len(emitErrs) > 0 {
			return joinErrs("protoc-gen-go-jet: emission failed", emitErrs)
		}

		// Final pass: generate go-jet model/table packages from the
		// ddl.sql files we just wrote. Group plans by their output
		// directory (one directory per proto package in the flat
		// layout); for each group, apply every ddl.sql to an ephemeral
		// PG and invoke jet's introspection-based generator. Without
		// this pass the aliases file's imports would dangle until the
		// developer ran a follow-up task — the plugin should be the
		// single entry point for "regenerate the storage layer".
		byDir := map[string][]*ResourcePlan{}
		for _, pe := range pending {
			byDir[pe.plan.GoDir] = append(byDir[pe.plan.GoDir], pe.plan)
		}
		dirs := make([]string, 0, len(byDir))
		for d := range byDir {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		// jet's generator prints progress via fmt.Println (→ os.Stdout),
		// but plugin-mode stdout is reserved for the CodeGeneratorResponse
		// protobuf. Route jet's noise to stderr for the duration of this
		// phase; defer restores stdout even on panic, so a crash in jet
		// can't corrupt the still-pending CodeGeneratorResponse that
		// protogen writes once our callback returns.
		origStdout := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = origStdout }()
		for _, dir := range dirs {
			absRoot := filepath.Join(repoRoot, dir)
			outDir := filepath.Join(absRoot, "jet")
			if err := runFromDDL(outDir, []string{absRoot}, distinctTenantRoles(byDir[dir]), jetColumnOverrides(byDir[dir])); err != nil {
				return fmt.Errorf("jet %s: %w", dir, err)
			}
		}
		return nil
	})
}

// rejectNestedTableAnnotations walks the nested messages of `m` and
// returns an error if any carries a `(storage.v1.table)` annotation.
// The plugin only resolves top-level messages, so a table annotation
// on a nested type silently does nothing — which is a footgun teams
// hit when copy-pasting annotations. Fail loudly at generation with
// a pointer at the fix (promote the message to the top level).
//
// Recurses so arbitrarily-deep nesting is covered, not just the
// immediate children. The `parentPath` argument builds a dotted
// trace in the error message so the offending position is obvious
// in a large .proto file.
func rejectNestedTableAnnotations(m *protogen.Message) error {
	var walk func(parentPath string, m *protogen.Message) error
	walk = func(parentPath string, m *protogen.Message) error {
		for _, child := range m.Messages {
			path := parentPath + "." + string(child.Desc.Name())
			if _, has := messageTable(child); has {
				return fmt.Errorf("%s: %s: nested message carries (storage.v1.table) but only top-level messages are resolved as persisted resources — promote the message to the top level of its .proto file, or drop the annotation",
					sourceLoc(child.Desc), path)
			}
			if err := walk(path, child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(string(m.Desc.Name()), m)
}

// jetColumnOverrides computes the (table, column) → Go type overrides
// jetgen needs for columns whose SQL type has no default scanner.
// Today the only case is TIMESTAMPTZ[] (emitted for `repeated
// Timestamp` fields) → `jettypes.TimestampArray`. Drop-in: new
// override kinds add a case here without touching jetgen or the
// runner.
func jetColumnOverrides(plans []*ResourcePlan) map[string]map[string]template.Type {
	out := map[string]map[string]template.Type{}
	for _, p := range plans {
		for _, c := range p.Columns {
			if c.Kind != KindRepeatedTimestamp {
				continue
			}
			if _, ok := out[p.TableName]; !ok {
				out[p.TableName] = map[string]template.Type{}
			}
			// Jet's `template.Type` lists the import separately but
			// renders `Name` verbatim at the field declaration site —
			// so the qualifier has to be baked into the name itself or
			// the generated struct ends up with a bare `TimestampArray`
			// that the Go compiler can't resolve.
			out[p.TableName][c.DBName] = template.Type{
				ImportPath: jettypesImport,
				Name:       "jettypes.TimestampArray",
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// jettypesImport points at the plugin-maintained helper package for
// types with custom Scan / Value methods. Centralised so the override
// wiring and the jet model field agree on the same import path.
const jettypesImport = "github.com/redpanda-data/protoc-gen-go-jet/pkg/pgstore/jettypes"

// distinctTenantRoles returns the unique set of RLS roles referenced by
// tenancy specs across the plans. Passed to runFromDDL so every role
// mentioned in ddl.sql policies exists before we apply the ddl.
func distinctTenantRoles(plans []*ResourcePlan) []string {
	seen := map[string]bool{}
	var roles []string
	for _, p := range plans {
		if p.Tenancy == nil || p.Tenancy.RuntimeRole == "" {
			continue
		}
		if seen[p.Tenancy.RuntimeRole] {
			continue
		}
		seen[p.Tenancy.RuntimeRole] = true
		roles = append(roles, p.Tenancy.RuntimeRole)
	}
	sort.Strings(roles)
	return roles
}

// columnSourceLoc returns the source location of the proto field
// backing a ColumnPlan. Synthesized columns (tenant_id, user_id) have
// no proto field — we fall back to a sentinel that still reads
// usefully in error output.
func columnSourceLoc(c ColumnPlan) string {
	if c.Field == nil {
		return "<synthesized>"
	}
	return sourceLoc(c.Field.Desc)
}

// sourceLoc returns a "<file>:<line>:<col>" string for a protobuf
// descriptor, suitable for compiler-style error prefixes that
// editors and CI log parsers link back to the source.
//
// Line/column come from source_code_info, which protoc attaches when
// parsing — protogen's Plugin.Files arrive with it already populated.
// ByDescriptor returns zero values for synthetic descriptors (we skip
// the colon in that case).
func sourceLoc(desc protoreflect.Descriptor) string {
	file := desc.ParentFile()
	if file == nil {
		return string(desc.FullName())
	}
	path := file.Path()
	loc := file.SourceLocations().ByDescriptor(desc)
	if loc.StartLine == 0 && loc.StartColumn == 0 {
		return path
	}
	return fmt.Sprintf("%s:%d:%d", path, loc.StartLine+1, loc.StartColumn+1)
}

// pendingEmit carries a resolved resource plus the proto source
// location that produced it — used by the cross-resource checks
// that run between resolve and emit.
type pendingEmit struct {
	source string
	plan   *ResourcePlan
}

// detectDuplicateTables flags pairs of resolved plans that would
// both emit `CREATE TABLE <name>` in the same Go storage directory.
// Two messages resolving to the same table name would collide at
// apply time with a cryptic "relation already exists" error from
// the drift-check or initial migration. Better to refuse at
// generate time with both source locations named.
//
// Scoped to (GoDir, TableName) rather than global: two different
// Go storage packages target different generated-Go trees (and
// typically different DBs), so a legitimate reuse of a table name
// across proto packages isn't a collision.
func detectDuplicateTables(pending []pendingEmit) []error {
	seen := make(map[string]string, len(pending))
	var errs []error
	for _, pe := range pending {
		key := pe.plan.GoDir + "|" + pe.plan.TableName
		if prev, collides := seen[key]; collides {
			errs = append(errs, fmt.Errorf("%s: table name %q collides with %s — two messages resolving to the same table in the same storage package would both emit CREATE TABLE at apply time; set `name:` on one to disambiguate", pe.source, pe.plan.TableName, prev))
			continue
		}
		seen[key] = pe.source
	}
	return errs
}

// joinErrs produces a single error that reads as a bullet list of every
// underlying error. Nicer for protoc/buf output than the first-error
// truncation.
func joinErrs(prefix string, errs []error) error {
	if len(errs) == 1 {
		return fmt.Errorf("%s: %w", prefix, errs[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d errors):", prefix, len(errs))
	for _, e := range errs {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return errors.New(b.String())
}

func emit(gen *protogen.Plugin, p *ResourcePlan) error {
	if err := emitSchemaSQL(p); err != nil {
		return fmt.Errorf("ddl.sql: %w", err)
	}
	emitMapper(gen, p)
	if err := emitSchema(gen, p); err != nil {
		return fmt.Errorf("aipjet schema: %w", err)
	}
	if err := emitAliases(p); err != nil {
		return fmt.Errorf("aliases: %w", err)
	}
	return nil
}

// readGoModulePath returns the module path declared in <repoRoot>/go.mod.
// The plugin maps each proto package's Go import path to a repo-relative
// output directory by stripping this prefix, so generated code lands in
// the consumer's module regardless of how that module is named.
func readGoModulePath(repoRoot string) (string, error) {
	body, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod (needed to resolve generated import paths): %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			if path := strings.TrimSpace(rest); path != "" {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("no `module` directive found in %s", filepath.Join(repoRoot, "go.mod"))
}
