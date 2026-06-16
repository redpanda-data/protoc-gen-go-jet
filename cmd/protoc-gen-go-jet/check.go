package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/sync/errgroup"

	pkgtestcontainers "github.com/redpanda-data/protoc-gen-go-jet/internal/testcontainers"
)

// tenantRolePattern restricts --tenant-role to PostgreSQL-safe unquoted
// identifier syntax. Defensive: the role name is operator-controlled,
// but it's interpolated into SQL literals and identifiers in
// bootstrapCheckPG, so reject anything that could break the DO $$ block.
var tenantRolePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// runCheck asserts that applying every ddl.sql under --ddl-roots
// produces the same schema as applying every migration under
// --migrations, for each plugin-managed table.
//
// Scope: "plugin-managed" = any table named in a `CREATE TABLE <name>`
// line inside a discovered ddl.sql. Tables only present on the
// migrations side (token_vault_*, operator-managed stuff) are
// invisible to the check — they're out of scope for the plugin.
func runCheck(argv []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var (
		ddlRoots      stringSlice
		migrationsDir string
		tenantRole    string
		resources     stringSlice
	)
	fs.Var(&ddlRoots, "ddl-roots", "comma-separated ddl.sql root directories (required, can be repeated)")
	fs.StringVar(&migrationsDir, "migrations", "", "path to forward-only migrations directory (required)")
	fs.StringVar(&tenantRole, "tenant-role", defaultTenantRole, "RLS role referenced by ddl.sql policies")
	fs.Var(&resources, "resources", "comma-separated proto message FQNs (e.g. example.v1.LLMProvider). When set, ddl.sql files whose `-- Resource:` header is not listed are ignored. Multiple apps sharing one proto package each pass their own subset.")
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Bring up the two PGs concurrently and populate them in parallel —
	// each container takes several seconds to start and neither half of
	// the drift check depends on the other. Serialising them doubled
	// every run. errgroup handles fan-out and returns the first error.
	var (
		pgA, pgB *pkgtestcontainers.Postgres
		dbA, dbB *sql.DB
	)
	bootstrap := new(errgroup.Group)
	bootstrap.Go(func() error {
		// PG-A: apply every ddl.sql → what proto says.
		var err error
		pgA, dbA, err = bootstrapCheckPG(ctx, tenantRole)
		if err != nil {
			return fmt.Errorf("bootstrap pg-A: %w", err)
		}
		return applyDDLFiles(ctx, dbA, pluginTables)
	})
	bootstrap.Go(func() error {
		// PG-B: apply forward migrations → what prod will look like.
		var err error
		pgB, dbB, err = bootstrapCheckPG(ctx, tenantRole)
		if err != nil {
			return fmt.Errorf("bootstrap pg-B: %w", err)
		}
		return applyMigrations(pgB.ConnectionString(), migrationsDir)
	})
	err = bootstrap.Wait()
	defer teardown(ctx, pgA, dbA)
	defer teardown(ctx, pgB, dbB)
	if err != nil {
		return err
	}

	// Diff per plugin-managed table. A plugin-managed table absent on
	// the migrations side (new resource, migration not authored yet)
	// is drift, not a runtime error — report it with a concrete next
	// step instead of a raw error.
	var failures []string
	for _, tbl := range pluginTables {
		expected, err := snapshotTable(ctx, dbA, tbl.Table)
		if err != nil {
			return fmt.Errorf("snapshot ddl side %s: %w", tbl.Table, err)
		}
		actual, err := snapshotTable(ctx, dbB, tbl.Table)
		if err != nil {
			return fmt.Errorf("snapshot migrations side %s: %w", tbl.Table, err)
		}
		if len(actual.Columns) == 0 {
			failures = append(failures, fmt.Sprintf("%s: missing on migrations side — add CREATE TABLE (copy from TARGET in the scaffolded .up.sql, or from %s)", tbl.Table, filepath.Base(tbl.DDLPath)))
			continue
		}
		if diff := cmp.Diff(expected, actual, cmp.AllowUnexported(SchemaSnapshot{}, TableSnapshot{}, Column{}, CheckConstraint{}, Index{}, Policy{})); diff != "" {
			failures = append(failures, fmt.Sprintf("%s:\n%s", tbl.Table, indent(diff, "    ")))
		}
	}
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "drift detected — migrations do not produce the schema ddl.sql declares")
		fmt.Fprintln(os.Stderr, "  (- lines: expected / what ddl.sql says; + lines: actual / what migrations produced)")
		fmt.Fprintln(os.Stderr)
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, f)
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Fix: `protoc-gen-go-jet diff` scaffolds an LLM")
		fmt.Fprintf(os.Stderr, "brief under %s with the structural DELTA and the concatenated\n", migrationsDir)
		fmt.Fprintln(os.Stderr, "TARGET ddl.sql. Author forward-only ALTER/CREATE/DROP beneath the")
		fmt.Fprintln(os.Stderr, "TODO(llm) marker, then re-run this check. Patterns:")
		fmt.Fprintln(os.Stderr, ".claude/skills/protoc-gen-go-jet/SKILL.md.")
		return fmt.Errorf("%d table(s) drifted", len(failures))
	}
	fmt.Printf("ok — %d plugin-managed table(s) match between ddl.sql and migrations\n", len(pluginTables))
	return nil
}

type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// --------------------------------------------------------------------
// Plugin-managed table discovery
// --------------------------------------------------------------------

type pluginManaged struct {
	DDLPath     string
	Table       string
	ResourceFQN string // Proto message FQN from the ddl.sql `-- Resource:` header, e.g. "example.v1.LLMProvider".
}

// discoverPluginTables finds every plugin-emitted ddl.sql under the
// given roots. The glob is single-level by design: the plugin writes
// one ddl.sql per resource directly into the storage package
// directory (flat layout). A recursive walk would pick up stray SQL
// files elsewhere in the tree.
//
// When resources is non-empty, only ddl files whose `-- Resource:`
// header FQN matches one of the listed entries are returned. Each
// listed FQN must resolve to exactly one ddl file — a typo catcher;
// silent mismatches would let a stale task invocation pass an empty
// check.
func discoverPluginTables(roots []string, resources []string) ([]pluginManaged, error) {
	var all []pluginManaged
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "*_ddl.sql"))
		if err != nil {
			return nil, err
		}
		for _, p := range matches {
			name, err := extractTableName(p)
			if err != nil {
				return nil, err
			}
			fqn, err := extractResourceFQN(p)
			if err != nil {
				return nil, err
			}
			all = append(all, pluginManaged{DDLPath: p, Table: name, ResourceFQN: fqn})
		}
	}

	if len(resources) == 0 {
		sort.Slice(all, func(i, j int) bool { return all[i].Table < all[j].Table })
		return all, nil
	}

	want := make(map[string]bool, len(resources))
	for _, r := range resources {
		want[r] = true
	}
	var out []pluginManaged
	seen := make(map[string]bool, len(resources))
	for _, t := range all {
		if !want[t.ResourceFQN] {
			continue
		}
		if seen[t.ResourceFQN] {
			return nil, fmt.Errorf("resource FQN %q matched more than one ddl.sql under %v", t.ResourceFQN, roots)
		}
		seen[t.ResourceFQN] = true
		out = append(out, t)
	}
	var missing []string
	for _, r := range resources {
		if !seen[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("--resources lists FQNs with no matching ddl.sql under %v: %v", roots, missing)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out, nil
}

// renameDirective is one parsed `-- PLUGIN-RENAME: <table> <old> -> <new>`
// line from a plugin-emitted DDL. The diff tool reads these to pre-align
// the migrations-side DB so pg-schema-diff doesn't see the rename as a
// DROP+ADD pair.
type renameDirective struct {
	Table string
	From  string
	To    string
}

// discoverRenameDirectives scans every plugin DDL under the given roots
// for PLUGIN-RENAME directives. Order within a table follows the DDL's
// emission order; across tables the output is stable because
// discoverPluginTables already sorts by table name.
func discoverRenameDirectives(tables []pluginManaged) ([]renameDirective, error) {
	var out []renameDirective
	for _, t := range tables {
		b, err := os.ReadFile(t.DDLPath) //nolint:gosec // plugin-discovered paths
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", t.DDLPath, err)
		}
		out = append(out, parseRenameDirectives(string(b))...)
	}
	return out, nil
}

// parseRenameDirectives extracts PLUGIN-RENAME lines from a DDL blob.
// Format emitted by emit_sql.go:
//
//	-- PLUGIN-RENAME: <table> <old> -> <new>
//
// Malformed lines are skipped — they indicate an outdated or
// hand-edited DDL; the declarative directive is advisory, so silent
// skip beats a hard error when the rest of the file is fine.
func parseRenameDirectives(ddl string) []renameDirective {
	const prefix = "-- PLUGIN-RENAME:"
	var out []renameDirective
	for _, line := range strings.Split(ddl, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		// Skip the header line (no "->" token) emitted above the
		// individual directives.
		arrow := strings.Index(body, " -> ")
		if arrow < 0 {
			continue
		}
		head := strings.Fields(body[:arrow])
		tail := strings.TrimSpace(body[arrow+len(" -> "):])
		if len(head) != 2 || tail == "" {
			continue
		}
		out = append(out, renameDirective{Table: head[0], From: head[1], To: tail})
	}
	return out
}

func extractTableName(ddlPath string) (string, error) {
	b, err := os.ReadFile(ddlPath) //nolint:gosec // ddl paths come from plugin discovery, not external input
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ddlPath, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "CREATE TABLE ") {
			rest := strings.TrimSpace(line[len("CREATE TABLE "):])
			rest = strings.TrimSuffix(rest, "(")
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("%s: no CREATE TABLE line", ddlPath)
}

// extractResourceFQN returns the proto message FQN from the `-- Resource:`
// header emitted by the plugin. Empty string (no error) when the header
// is absent — e.g. legacy ddl.sql files from before the header was
// introduced. Callers that filter by --resources must handle that case
// by listing the empty string (they wouldn't) or by expecting zero
// matches and erroring out via discoverPluginTables' typo check.
func extractResourceFQN(ddlPath string) (string, error) {
	b, err := os.ReadFile(ddlPath) //nolint:gosec // ddl paths come from plugin discovery, not external input
	if err != nil {
		return "", fmt.Errorf("read %s: %w", ddlPath, err)
	}
	const prefix = "-- Resource:"
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):]), nil
		}
		// Stop scanning once past the leading comment block.
		if line != "" && !strings.HasPrefix(line, "--") {
			break
		}
	}
	return "", nil
}

// --------------------------------------------------------------------
// Apply
// --------------------------------------------------------------------

func applyDDLFiles(ctx context.Context, db *sql.DB, tables []pluginManaged) error {
	// Topo-sort by inline FK references before applying. Each DDL
	// file is self-contained (CREATE TABLE with inline FK CONSTRAINT),
	// so the only ordering requirement is "referenced table's file
	// before referrer's file." topoSortDDL extracts that graph by
	// grepping REFERENCES clauses out of the file text — no
	// additional metadata needed beyond what's already in the SQL.
	files := make([]FileContent, 0, len(tables))
	for _, t := range tables {
		b, err := os.ReadFile(t.DDLPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", t.DDLPath, err)
		}
		files = append(files, FileContent{Path: t.DDLPath, Content: string(b)})
	}
	ordered, err := topoSortDDL(files)
	if err != nil {
		return fmt.Errorf("topo-sort ddl files: %w", err)
	}
	for _, f := range ordered {
		if _, err := db.ExecContext(ctx, f.Content); err != nil {
			return fmt.Errorf("exec %s: %w", f.Path, err)
		}
	}
	return nil
}

func applyMigrations(connstr, migrationsDir string) error {
	abs, err := filepath.Abs(migrationsDir)
	if err != nil {
		return fmt.Errorf("abs %s: %w", migrationsDir, err)
	}
	src, err := (&file.File{}).Open("file://" + abs)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	m, err := migrate.NewWithSourceInstance("file", src, connstr)
	if err != nil {
		return fmt.Errorf("new migrate: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("up: %w", err)
	}
	return nil
}

// --------------------------------------------------------------------
// Container lifecycle
// --------------------------------------------------------------------

// bootstrapCheckPG brings up a disposable Postgres and creates the
// tenant role named in the RLS policies. The role gets CONNECT +
// USAGE on public only — not table-level grants. Both sides of the
// drift check (ddl.sql apply and migrations apply) run as the
// superuser, so the tenant role never actually performs reads or
// writes. It only needs to exist so `CREATE POLICY ... TO "<role>"`
// doesn't fail with "role does not exist". Adding table grants here
// would be dead code; leaving them out keeps the ephemeral container
// startup fast.
func bootstrapCheckPG(ctx context.Context, tenantRole string) (*pkgtestcontainers.Postgres, *sql.DB, error) {
	if !tenantRolePattern.MatchString(tenantRole) {
		return nil, nil, fmt.Errorf("invalid --tenant-role %q: must match %s", tenantRole, tenantRolePattern)
	}
	pg, err := pkgtestcontainers.NewPostgres() //nolint:contextcheck // pkg helper uses its own context.Background
	if err != nil {
		return nil, nil, fmt.Errorf("start pg: %w", err)
	}
	super, err := pgx.Connect(ctx, pg.ConnectionString())
	if err != nil {
		return pg, nil, fmt.Errorf("connect super: %w", err)
	}
	// tenantRole is validated above to contain only [A-Za-z0-9_-], so the
	// direct interpolation into the SQL string and identifier slots is safe.
	_, err = super.Exec(ctx, fmt.Sprintf(`
		DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
				CREATE ROLE "%[1]s" LOGIN PASSWORD '%[1]s';
			END IF;
		END $$;
		GRANT CONNECT ON DATABASE postgres TO "%[1]s";
		GRANT USAGE ON SCHEMA public TO "%[1]s";
	`, tenantRole))
	_ = super.Close(ctx)
	if err != nil {
		return pg, nil, fmt.Errorf("create tenant role: %w", err)
	}
	db, err := sql.Open("pgx", pg.ConnectionString())
	if err != nil {
		return pg, nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return pg, db, err
	}
	return pg, db, nil
}

func teardown(ctx context.Context, pg *pkgtestcontainers.Postgres, db *sql.DB) {
	if db != nil {
		_ = db.Close()
	}
	if pg != nil {
		_ = pg.Terminate(ctx)
	}
}

// --------------------------------------------------------------------
// Schema snapshots
// --------------------------------------------------------------------

// SchemaSnapshot is the structural view introspected per table. The
// exported type and its children make cmp.Diff's output readable when
// drift is reported.
type SchemaSnapshot struct {
	Tables map[string]TableSnapshot
}

type TableSnapshot struct {
	Columns         []Column
	PrimaryKey      []string
	CheckConstraint []CheckConstraint
	Index           []Index
	RLSEnabled      bool
	Policy          []Policy
	TableComment    string
	ForeignKey      []ForeignKey
}

// ForeignKey is the introspected view of a FK constraint. Body is
// pg_get_constraintdef's canonical rendering, which normalises action
// spellings, column ordering inside the parens, and MATCH clauses so
// two databases with semantically-equivalent constraints compare
// equal via cmp.Diff even when the original DDL differed in
// formatting.
//
// HasBackingIndex is pre-computed against pg_indexes so drift-check
// can flag "FK exists but is unindexed" as a distinct issue — an
// unindexed FK is a parent-side UPDATE/DELETE perf trap that the
// plugin treats as drift rather than a config choice.
type ForeignKey struct {
	Name            string
	Body            string
	Validated       bool
	HasBackingIndex bool
}

type Column struct {
	Name        string
	SQLType     string
	NotNull     bool
	DefaultExpr string
	Comment     string
}

type CheckConstraint struct {
	Name string
	Body string
}

type Index struct {
	Name string
	SQL  string
}

type Policy struct {
	Name      string
	Role      string
	Using     string
	WithCheck string
	ForCmd    string
}

// snapshotTable introspects a Postgres table into a structural
// TableSnapshot. Absent tables produce a zero-valued snapshot (empty
// Columns) rather than an error — callers distinguish the two via
// `len(ts.Columns) == 0`. The `information_schema.columns` query over
// a non-existent table returns zero rows without error, and no real
// table can have zero columns, so the signal is unambiguous.
func snapshotTable(ctx context.Context, db *sql.DB, table string) (TableSnapshot, error) {
	ts := TableSnapshot{}
	cols, err := fetchColumns(ctx, db, table)
	if err != nil {
		return ts, err
	}
	if len(cols) == 0 {
		return ts, nil
	}
	// Order columns by name before comparison. Postgres doesn't treat
	// column order as semantic (projections are by name) and ALTER
	// TABLE ADD COLUMN always appends — requiring drift-free order
	// would mean rebuilding tables for every new field. Compare the
	// set, not the sequence.
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	ts.Columns = cols
	if ts.PrimaryKey, err = fetchPrimaryKey(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.CheckConstraint, err = fetchChecks(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.Index, err = fetchIndexes(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.RLSEnabled, err = fetchRLSEnabled(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.Policy, err = fetchPolicies(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.TableComment, err = fetchTableComment(ctx, db, table); err != nil {
		return ts, err
	}
	if ts.ForeignKey, err = fetchForeignKeys(ctx, db, table); err != nil {
		return ts, err
	}
	return ts, nil
}

// fetchForeignKeys returns every FK constraint rooted on `table`. The
// query reads pg_constraint directly because information_schema's
// referential_constraints view splits a single constraint across
// multiple rows (one per column), and reassembling the canonical
// clause body from them is strictly worse than asking PG for it via
// pg_get_constraintdef.
//
// A separate pg_indexes probe determines HasBackingIndex — a btree
// whose leading column list exactly equals the FK's local columns.
// Weaker coverage (e.g. FK on `col` with an index on `(other, col)`)
// would not speed parent-side UPDATE/DELETE lookups, so we require
// the strict prefix match.
func fetchForeignKeys(ctx context.Context, db *sql.DB, table string) ([]ForeignKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			c.conname,
			pg_get_constraintdef(c.oid),
			c.convalidated,
			array_to_string(c.conkey::int[], ',')
		FROM pg_constraint c
		JOIN pg_class t ON c.conrelid = t.oid
		WHERE c.contype = 'f'
		  AND t.relnamespace = 'public'::regnamespace
		  AND t.relname = $1
		ORDER BY c.conname
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawFK struct {
		name      string
		body      string
		validated bool
		cols      []int
	}
	var raws []rawFK
	for rows.Next() {
		var name, def, conkey string
		var validated bool
		if err := rows.Scan(&name, &def, &validated, &conkey); err != nil {
			return nil, err
		}
		cols, err := parseIntCSV(conkey)
		if err != nil {
			return nil, fmt.Errorf("parse conkey %q: %w", conkey, err)
		}
		// pg_get_constraintdef renders FKs as "FOREIGN KEY (cols) ..."
		// with a trailing space after the keyword. Strip to the
		// parenthesised body so the snapshot field is stable across
		// PG versions that might whitespace-normalise differently.
		body := strings.TrimSpace(strings.TrimPrefix(def, "FOREIGN KEY "))
		raws = append(raws, rawFK{name: name, body: body, validated: validated, cols: cols})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	colNames, err := fetchColumnAttnames(ctx, db, table)
	if err != nil {
		return nil, err
	}
	out := make([]ForeignKey, 0, len(raws))
	for _, raw := range raws {
		var localCols []string
		for _, idx := range raw.cols {
			name, ok := colNames[idx]
			if !ok {
				return nil, fmt.Errorf("FK %q references attnum %d not present in %s's column map", raw.name, idx, table)
			}
			localCols = append(localCols, name)
		}
		hasIdx, err := hasBackingIndex(ctx, db, table, localCols)
		if err != nil {
			return nil, err
		}
		out = append(out, ForeignKey{
			Name:            raw.name,
			Body:            raw.body,
			Validated:       raw.validated,
			HasBackingIndex: hasIdx,
		})
	}
	return out, nil
}

// parseIntCSV parses "1,2,3" into [1, 2, 3]. Defensive against the
// rare edge case where an FK has no local columns (pg_constraint
// rows can have empty conkey for certain constraint types — not
// possible for contype='f' today, but cheap to handle).
func parseIntCSV(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// fetchColumnAttnames returns a map of attnum -> column name for the
// given table. Used to turn pg_constraint's conkey attnum array into
// SQL column identifiers without a second round-trip per FK.
func fetchColumnAttnames(ctx context.Context, db *sql.DB, table string) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.attnum, a.attname
		FROM pg_attribute a
		JOIN pg_class t ON a.attrelid = t.oid
		WHERE t.relname = $1
		  AND t.relnamespace = 'public'::regnamespace
		  AND a.attnum > 0
		  AND NOT a.attisdropped
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var num int
		var name string
		if err := rows.Scan(&num, &name); err != nil {
			return nil, err
		}
		out[num] = name
	}
	return out, rows.Err()
}

// hasBackingIndex reports whether `table` has a btree index whose
// leading columns exactly match `localCols` in order. A composite
// index on `(a, b, c)` counts as backing `[a]` and `[a, b]` but not
// `[b]` — btrees only accelerate prefix lookups. Unique indexes
// count; expression-only indexes do not.
func hasBackingIndex(ctx context.Context, db *sql.DB, table string, localCols []string) (bool, error) {
	if len(localCols) == 0 {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT array_to_string(
			array(
				SELECT a.attname
				FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
				JOIN pg_attribute a
				  ON a.attrelid = i.indrelid AND a.attnum = k.attnum
				ORDER BY k.ord
			), ',')
		FROM pg_index i
		JOIN pg_class c ON i.indexrelid = c.oid
		JOIN pg_class t ON i.indrelid = t.oid
		JOIN pg_am am ON c.relam = am.oid
		WHERE t.relname = $1
		  AND t.relnamespace = 'public'::regnamespace
		  AND am.amname = 'btree'
	`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	prefix := strings.Join(localCols, ",")
	for rows.Next() {
		var cols string
		if err := rows.Scan(&cols); err != nil {
			return false, err
		}
		// Leading-column match: index columns must START with the FK's
		// local columns in the same order. An index on exactly the FK
		// columns qualifies; a superset index starting with them also
		// qualifies; any other shape does not.
		if cols == prefix || strings.HasPrefix(cols, prefix+",") {
			return true, nil
		}
	}
	return false, rows.Err()
}

func fetchTableComment(ctx context.Context, db *sql.DB, table string) (string, error) {
	var comment sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT obj_description(c.oid, 'pg_class')
		FROM pg_class c
		WHERE c.relname = $1
		  AND c.relnamespace = 'public'::regnamespace
	`, table).Scan(&comment)
	if err != nil {
		return "", err
	}
	return comment.String, nil
}

func fetchColumns(ctx context.Context, db *sql.DB, table string) ([]Column, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			c.column_name,
			c.data_type,
			c.udt_name,
			c.is_nullable,
			COALESCE(c.column_default, ''),
			COALESCE(pgd.description, '')
		FROM information_schema.columns c
		LEFT JOIN pg_catalog.pg_statio_all_tables st
		  ON st.schemaname = c.table_schema AND st.relname = c.table_name
		LEFT JOIN pg_catalog.pg_description pgd
		  ON pgd.objoid = st.relid AND pgd.objsubid = c.ordinal_position
		WHERE c.table_schema = 'public' AND c.table_name = $1
		ORDER BY c.ordinal_position
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var name, dataType, udt, isNullable, dflt, comment string
		if err := rows.Scan(&name, &dataType, &udt, &isNullable, &dflt, &comment); err != nil {
			return nil, err
		}
		out = append(out, Column{
			Name:        name,
			SQLType:     normaliseSQLType(dataType, udt),
			NotNull:     isNullable == "NO",
			DefaultExpr: normaliseDefault(dflt),
			Comment:     comment,
		})
	}
	return out, rows.Err()
}

// normaliseSQLType renders Postgres's information_schema / pg_catalog
// type metadata back to the SQL spelling the plugin emits in ddl.sql.
// Without this, the drift check compares "ARRAY" vs "TEXT[]" and
// reports false drift every time.
//
// Array types surface in information_schema with data_type="ARRAY" and
// a udt_name that starts with an underscore followed by the element's
// internal PG name (`_text`, `_int4`, `_float8`, ...). Map the handful
// the plugin actually emits; anything else falls through to uppercase
// so a new kind surfaces as drift rather than sneaking past.
func normaliseSQLType(dataType, udtName string) string {
	if dataType == "ARRAY" {
		switch udtName {
		case "_text":
			return "TEXT[]"
		case "_bool":
			return "BOOLEAN[]"
		case "_int4":
			return "INTEGER[]"
		case "_int8":
			return "BIGINT[]"
		case "_float4":
			return "REAL[]"
		case "_float8":
			return "DOUBLE PRECISION[]"
		case "_bytea":
			return "BYTEA[]"
		case "_timestamptz":
			return "TIMESTAMPTZ[]"
		}
	}
	switch dataType {
	case "text":
		return "TEXT"
	case "jsonb":
		return "JSONB"
	case "timestamp with time zone":
		return "TIMESTAMPTZ"
	case "boolean":
		return "BOOLEAN"
	case "integer":
		return "INTEGER"
	case "bigint":
		return "BIGINT"
	case "bytea":
		return "BYTEA"
	case "real":
		return "REAL"
	case "double precision":
		return "DOUBLE PRECISION"
	}
	return strings.ToUpper(dataType)
}

func normaliseDefault(s string) string {
	if i := strings.Index(s, "::"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func fetchPrimaryKey(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = 'public'
		  AND tc.table_name = $1
		ORDER BY kcu.ordinal_position
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func fetchChecks(ctx context.Context, db *sql.DB, table string) ([]CheckConstraint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.conname, pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON c.conrelid = t.oid
		WHERE c.contype = 'c'
		  AND t.relnamespace = 'public'::regnamespace
		  AND t.relname = $1
		ORDER BY c.conname
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckConstraint
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return nil, err
		}
		body := strings.TrimPrefix(def, "CHECK ")
		body = stripOuterParens(strings.TrimSpace(body))
		out = append(out, CheckConstraint{Name: name, Body: body})
	}
	return out, rows.Err()
}

func stripOuterParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		balanced := true
		for i := 0; i < len(s)-1; i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					balanced = false
				}
			}
			if !balanced {
				break
			}
		}
		if !balanced {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func fetchIndexes(ctx context.Context, db *sql.DB, table string) ([]Index, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = $1
		  AND indexname NOT LIKE '%_pkey'
		ORDER BY indexname
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return nil, err
		}
		out = append(out, Index{Name: name, SQL: normaliseIndexDef(def)})
	}
	return out, rows.Err()
}

func normaliseIndexDef(s string) string {
	s = strings.ReplaceAll(s, " public.", " ")
	return strings.Join(strings.Fields(s), " ")
}

func fetchRLSEnabled(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var enabled bool
	err := db.QueryRowContext(ctx, `
		SELECT relrowsecurity FROM pg_class
		WHERE relname = $1 AND relnamespace = 'public'::regnamespace
	`, table).Scan(&enabled)
	return enabled, err
}

func fetchPolicies(ctx context.Context, db *sql.DB, table string) ([]Policy, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			p.polname,
			CASE p.polcmd
				WHEN '*' THEN 'ALL'
				WHEN 'r' THEN 'SELECT'
				WHEN 'a' THEN 'INSERT'
				WHEN 'w' THEN 'UPDATE'
				WHEN 'd' THEN 'DELETE'
				ELSE p.polcmd::text
			END,
			COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
			COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), ''),
			COALESCE(
				(SELECT string_agg(quote_ident(r.rolname), ',' ORDER BY r.rolname)
				 FROM pg_roles r WHERE r.oid = ANY(p.polroles)),
				''
			)
		FROM pg_policy p
		JOIN pg_class t ON p.polrelid = t.oid
		WHERE t.relname = $1 AND t.relnamespace = 'public'::regnamespace
		ORDER BY p.polname
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var name, cmd, using, withCheck, role string
		if err := rows.Scan(&name, &cmd, &using, &withCheck, &role); err != nil {
			return nil, err
		}
		out = append(out, Policy{
			Name:      name,
			Role:      role,
			Using:     stripOuterParens(using),
			WithCheck: stripOuterParens(withCheck),
			ForCmd:    cmd,
		})
	}
	return out, rows.Err()
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
