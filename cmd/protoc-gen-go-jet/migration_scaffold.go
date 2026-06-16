package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	pkgtestcontainers "github.com/redpanda-data/protoc-gen-go-jet/internal/testcontainers"
)

// runDiff emits a SQL migration scaffold for the LLM to author.
//
// Two modes, dispatched on the state of the migrations directory:
//
//   - Initial mode — migrations directory is empty (first-ever
//     migration for this DB). The plugin emits the concatenated
//     canonical ddl.sql files as the body, stripped of their DO NOT
//     EDIT headers. No LLM involvement needed; a human `buf generate`
//     produced the ddl already, and the first migration's whole job is
//     to reproduce it.
//
//   - Brief mode — one or more non-empty migrations already exist.
//     The plugin emits a structured brief: current migration-chain
//     schema (pg_dump on an ephemeral PG that has all existing
//     migrations applied), target ddl.sql, active PLUGIN-* directives,
//     and a TODO(llm) marker. The LLM / operator authors the ALTER
//     SQL beneath the brief. protoc-gen-go-jet check remains the guardrail
//     that fails if the authored SQL doesn't converge on ddl.sql.
//
// This replaces the prior pg-schema-diff integration, which emitted
// several classes of invalid SQL (see git log) and had permanent
// structural limits (composite types, RLS policy bodies, custom
// types). LLM authoring trades "sometimes auto-done" for "always
// hand-auditable," with the same drift-check safety net on the tail.
func runDiff(argv []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	var (
		ddlRoots      stringSlice
		migrationsDir string
		tenantRole    string
		resources     stringSlice
	)
	fs.Var(&ddlRoots, "ddl-roots", "comma-separated ddl.sql root directories (required, can be repeated)")
	fs.StringVar(&migrationsDir, "migrations", "", "path to forward-only migrations directory (required)")
	fs.StringVar(&tenantRole, "tenant-role", defaultTenantRole, "RLS role referenced by ddl.sql policies")
	fs.Var(&resources, "resources", "comma-separated proto message FQNs — filter ddl.sql files by their `-- Resource:` header. Same semantics as `check`.")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if len(ddlRoots) == 0 {
		return errors.New("--ddl-roots is required")
	}
	if migrationsDir == "" {
		return errors.New("--migrations is required")
	}

	pluginTables, err := discoverPluginTables(ddlRoots, resources)
	if err != nil {
		return fmt.Errorf("discover plugin tables: %w", err)
	}
	if len(pluginTables) == 0 {
		if len(resources) > 0 {
			return fmt.Errorf("no plugin-managed tables matched --resources=%v under %v", []string(resources), []string(ddlRoots))
		}
		return fmt.Errorf("no plugin-managed tables found under %v", []string(ddlRoots))
	}

	hasExisting, err := hasNonEmptyMigrations(migrationsDir)
	if err != nil {
		return fmt.Errorf("scan migrations dir: %w", err)
	}

	if !hasExisting {
		return emitInitialFromDDL(pluginTables)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return emitLLMBrief(ctx, pluginTables, migrationsDir, tenantRole)
}

// hasNonEmptyMigrations reports whether the directory already contains
// any `.up.sql` file with content. Empty scaffolds (the placeholder
// `migrate create` just emitted, which we're about to fill) don't
// count — otherwise the first-ever diff invocation would skip the
// initial-migration path. golang-migrate ignores empty files too.
func hasNonEmptyMigrations(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return false, err
		}
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// emitInitialFromDDL writes the migration preamble followed by each
// plugin-managed ddl.sql, topo-sorted so every referring table's
// CREATE TABLE lands after its referent's, with the generator-header
// boilerplate stripped. The concatenated result IS the first
// migration — applying it reproduces the canonical schema 1:1, which
// is exactly what drift-check will then verify.
//
// Topo-sorting matters because go-migrate applies the file as a
// single sequential script. Without it, alphabetic filename order
// (agent < mcp_server) would drive CREATE TABLE agent with an inline
// FK to mcp_server before mcp_server exists — applying fails with
// "relation does not exist."
func emitInitialFromDDL(tables []pluginManaged) error {
	files := make([]FileContent, 0, len(tables))
	for _, t := range tables {
		b, err := os.ReadFile(t.DDLPath) //nolint:gosec // plugin-discovered paths
		if err != nil {
			return fmt.Errorf("read %s: %w", t.DDLPath, err)
		}
		files = append(files, FileContent{Path: t.DDLPath, Content: string(b)})
	}
	ordered, err := topoSortDDL(files)
	if err != nil {
		return fmt.Errorf("topo-sort ddl files: %w", err)
	}
	fmt.Println(migrationPreamble())
	fmt.Println()
	for i, f := range ordered {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("-- from %s\n", filepath.Base(f.Path))
		fmt.Print(stripDDLHeader(f.Content))
	}
	return nil
}

// stripDDLHeader removes the leading "DO NOT EDIT" header block the
// plugin writes on every generated ddl.sql. The first migration is a
// human-edited artifact (well, a copy of a generated one at a point in
// time) — the do-not-edit disclaimer belongs on the source file, not
// on the migration that happens to start as a snapshot of it.
func stripDDLHeader(s string) string {
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "--") {
			i++
			continue
		}
		break
	}
	return strings.Join(lines[i:], "\n")
}

// emitLLMBrief bootstraps two ephemeral PGs (one for the migration
// chain, one for the canonical ddl.sql), snapshots every plugin-managed
// table on both sides, and renders a structured per-table delta above
// the TARGET-side ddl concatenation. The operator then authors the
// migration body beneath the TODO marker; protoc-gen-go-jet check re-runs the
// same comparison and fails if the authored SQL doesn't close the delta.
//
// Design choice: surface the delta, not two dump dumps. A full
// pg_dump of the CURRENT side balloons the brief with irrelevant
// boilerplate (SETs, search_path, per-catalog-comment churn) and
// leaves the operator to eyeball a diff. The structured delta names
// exactly which columns / checks / indexes / policies / comments
// differ, in stable order, so the author's job is "write the ALTERs
// that close this list" — no diffing required.
func emitLLMBrief(ctx context.Context, tables []pluginManaged, migrationsDir, tenantRole string) error {
	var (
		pgM, pgD *pkgtestcontainers.Postgres
		dbM, dbD *sql.DB
	)
	g := new(errgroup.Group)
	g.Go(func() error {
		var err error
		pgM, dbM, err = bootstrapCheckPG(ctx, tenantRole)
		if err != nil {
			return fmt.Errorf("bootstrap migrations pg: %w", err)
		}
		if err := applyMigrations(pgM.ConnectionString(), migrationsDir); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var err error
		pgD, dbD, err = bootstrapCheckPG(ctx, tenantRole)
		if err != nil {
			return fmt.Errorf("bootstrap ddl pg: %w", err)
		}
		if err := applyDDLFiles(ctx, dbD, tables); err != nil {
			return fmt.Errorf("apply ddl: %w", err)
		}
		return nil
	})
	err := g.Wait()
	defer teardown(ctx, pgM, dbM)
	defer teardown(ctx, pgD, dbD)
	if err != nil {
		return err
	}

	// Per-table structural delta. The delta is intentionally naive:
	// a renamed column shows up as `- column old` + `+ column new`.
	// Reconciliation with PLUGIN-RENAME directives (printed above)
	// happens in the reader's head — see SKILL.md's "Active
	// PLUGIN-RENAME directives" section.
	var delta []string
	for _, t := range tables {
		tgt, err := snapshotTable(ctx, dbD, t.Table)
		if err != nil {
			return fmt.Errorf("snapshot ddl side %s: %w", t.Table, err)
		}
		cur, err := snapshotTable(ctx, dbM, t.Table)
		if err != nil {
			return fmt.Errorf("snapshot migrations side %s: %w", t.Table, err)
		}
		if len(cur.Columns) == 0 {
			delta = append(delta, fmt.Sprintf("+ table %s (new — see TARGET for full CREATE)", t.Table))
			continue
		}
		delta = append(delta, computeTableDelta(t.Table, cur, tgt)...)
	}

	directives, err := discoverRenameDirectives(tables)
	if err != nil {
		return fmt.Errorf("discover rename directives: %w", err)
	}

	// TARGET = concatenated ddl.sql. Still the source of truth the
	// author cross-references when writing non-trivial ALTERs
	// (constraint bodies, CREATE POLICY shapes, etc.).
	var targetDDL strings.Builder
	for _, t := range tables {
		body, err := os.ReadFile(t.DDLPath) //nolint:gosec // plugin-discovered paths
		if err != nil {
			return fmt.Errorf("read %s: %w", t.DDLPath, err)
		}
		fmt.Fprintf(&targetDDL, "-- from %s\n", filepath.Base(t.DDLPath))
		content := stripDDLHeader(string(body))
		targetDDL.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			targetDDL.WriteString("\n")
		}
	}

	fmt.Println(migrationPreamble())
	fmt.Println()
	fmt.Println("-- ======================================================================")
	fmt.Println("-- LLM BRIEF — author the migration body below, then run `protoc-gen-go-jet check`")
	fmt.Println("-- ======================================================================")
	fmt.Println("--")
	fmt.Println("-- See .claude/skills/protoc-gen-go-jet/SKILL.md for authoring patterns")
	fmt.Println("-- (rename_from, column drops, NOT VALID CHECK idiom, CONCURRENTLY + no-tx,")
	fmt.Println("--  RLS policy changes, etc.).")
	fmt.Println("--")
	if len(directives) > 0 {
		fmt.Println("-- Active PLUGIN-RENAME directives (author as ALTER ... RENAME COLUMN):")
		for _, d := range directives {
			fmt.Printf("--   %s.%s -> %s\n", d.Table, d.From, d.To)
		}
		fmt.Println("--")
	}
	fmt.Println("-- ------------------- DELTA (what needs to change) ---------------------")
	if len(delta) == 0 {
		fmt.Println("-- (no structural delta — likely a comment-only or no-op change)")
	} else {
		for _, d := range delta {
			fmt.Println("-- " + d)
		}
	}
	fmt.Println("--")
	fmt.Println("-- ------------------- TARGET (canonical ddl.sql) -----------------------")
	commentBlock(targetDDL.String())
	fmt.Println("-- ======================================================================")
	fmt.Println()
	fmt.Println("-- TODO(llm): author the forward-only ALTER / CREATE / DROP below that")
	fmt.Println("-- closes the DELTA. protoc-gen-go-jet check will fail until the migration chain")
	fmt.Println("-- produces the same shape as ddl.sql.")
	return nil
}

// commentBlock prints a multi-line string prefixed with `-- ` on every
// line so the whole block remains SQL-comment content.
func commentBlock(s string) {
	for _, line := range strings.Split(s, "\n") {
		fmt.Println("-- " + line)
	}
}

// computeTableDelta emits one line per named object that differs
// between `current` and `target`, tagged `+` (only in target),
// `-` (only in current), or `~` (both sides, signature differs).
// The line carries just the object kind and name; the reader
// cross-references TARGET for the new shape.
//
// Deliberately mechanical: no attempt to reconcile renames,
// classify hazards, or explain what changed. Those judgement
// calls belong in the reader (SKILL.md teaches the patterns),
// not in generator code that can't see authorial intent.
func computeTableDelta(table string, current, target TableSnapshot) []string {
	var out []string
	out = append(out, diffNamed(table, "column", indexBy(current.Columns, func(c Column) string { return c.Name }), indexBy(target.Columns, func(c Column) string { return c.Name }))...)
	out = append(out, diffNamed(table, "check", indexBy(current.CheckConstraint, func(c CheckConstraint) string { return c.Name }), indexBy(target.CheckConstraint, func(c CheckConstraint) string { return c.Name }))...)
	out = append(out, diffNamed(table, "index", indexBy(current.Index, func(i Index) string { return i.Name }), indexBy(target.Index, func(i Index) string { return i.Name }))...)
	out = append(out, diffNamed(table, "policy", indexBy(current.Policy, func(p Policy) string { return p.Name }), indexBy(target.Policy, func(p Policy) string { return p.Name }))...)

	// Foreign keys. A `+ fk` line on a pre-existing table is a
	// populated-table constraint add — under the default one-shot
	// form it takes ACCESS EXCLUSIVE while PG validates every
	// existing row. Flag with a HAZARD and the two-step idiom so
	// the author knows to reach for NOT VALID + VALIDATE CONSTRAINT
	// instead of blocking production. Fresh tables (handled in the
	// caller via `len(cur.Columns) == 0`) never reach this path, so
	// every + here refers to an already-populated target.
	fkLines := diffNamed(table, "fk",
		indexBy(current.ForeignKey, func(f ForeignKey) string { return f.Name }),
		indexBy(target.ForeignKey, func(f ForeignKey) string { return f.Name }))
	for _, line := range fkLines {
		out = append(out, line)
		if strings.HasPrefix(line, "+ fk ") {
			out = append(out,
				fmt.Sprintf("  HAZARD: %s — ADD CONSTRAINT takes ACCESS EXCLUSIVE + full table scan. Author as `ALTER TABLE %s ADD CONSTRAINT <name> FOREIGN KEY ... NOT VALID;` then `ALTER TABLE %s VALIDATE CONSTRAINT <name>;` in a follow-up migration.", table, table, table))
		}
	}

	if strings.Join(current.PrimaryKey, ",") != strings.Join(target.PrimaryKey, ",") {
		out = append(out, fmt.Sprintf("~ primary_key %s", table))
	}
	if current.RLSEnabled != target.RLSEnabled {
		out = append(out, fmt.Sprintf("~ rls %s", table))
	}
	if current.TableComment != target.TableComment {
		out = append(out, fmt.Sprintf("~ table_comment %s", table))
	}
	return out
}

// diffNamed walks the union of keys in cur and tgt, sorted, emitting
// +/-/~ lines. Uses Go's `==` on the values — every snapshot struct
// (Column, CheckConstraint, Index, Policy) has only comparable fields,
// so the equality check catches every field difference without a
// separator-delimited signature string that could false-positive on
// pipe-bearing comments or policy expressions.
func diffNamed[T comparable](table, kind string, cur, tgt map[string]T) []string {
	names := map[string]struct{}{}
	for n := range cur {
		names[n] = struct{}{}
	}
	for n := range tgt {
		names[n] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	var out []string
	for _, n := range ordered {
		c, inC := cur[n]
		t, inT := tgt[n]
		switch {
		case inT && !inC:
			out = append(out, fmt.Sprintf("+ %s %s.%s", kind, table, n))
		case inC && !inT:
			out = append(out, fmt.Sprintf("- %s %s.%s", kind, table, n))
		case c != t:
			out = append(out, fmt.Sprintf("~ %s %s.%s", kind, table, n))
		}
	}
	return out
}

func indexBy[T any](items []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(items))
	for _, it := range items {
		m[key(it)] = it
	}
	return m
}

// migrationPreamble returns the boilerplate header the diff subcommand
// prints at the top of every scaffolded .up.sql.
func migrationPreamble() string {
	return `-- Forward-only migration scaffolded by ` + "`protoc-gen-go-jet diff`" + `.
-- Initial migrations are a 1:1 copy of the generated ddl.sql files.
-- Subsequent migrations are authored beneath an LLM brief (see
-- ` + "`.claude/skills/protoc-gen-go-jet/SKILL.md`" + `). ` + "`protoc-gen-go-jet check`" + `
-- re-runs the full migration chain and fails if it diverges from
-- ddl.sql, so the LLM-authored SQL is always machine-verified.`
}
