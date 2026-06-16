package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// emitSchemaSQL writes the canonical ddl.sql for this resource —
// the full CREATE TABLE + indexes + CHECK + RLS representation of the
// current proto state. Overwritten on every regen; it is a reference
// document, not an apply-able migration.
//
// Bootstrapping a new resource: `cp ddl.sql migrations/sql/<db>/NNNN_<table>_initial.up.sql`.
// Subsequent changes: `git diff HEAD -- ddl.sql` shows the SQL delta
// to author as an ALTER migration. The plugin never writes to
// migration_dir — migrations are 100% hand-authored forward-only.
// The TestMigrationsMatchDDL integration test compares the two paths
// per-table and fails if they drift.
func emitSchemaSQL(p *ResourcePlan) error {
	abs := filepath.Join(p.RepoRoot, p.GoDir, p.FilenamePrefix+"_ddl.sql")

	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}

	sql := renderSchemaSQL(p)
	if err := os.WriteFile(abs, []byte(sql), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", abs, err)
	}
	return nil
}

// renderSchemaSQL is also reused by the drift test for CHECK / index
// expectations — keep it deterministic in column and constraint ordering.
func renderSchemaSQL(p *ResourcePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Code generated from %s by protoc-gen-go-jet. DO NOT EDIT.\n", p.ProtoFile)
	fmt.Fprint(&b, "--\n")
	if p.ResourceFQN != "" {
		fmt.Fprintf(&b, "-- Resource: %s\n", p.ResourceFQN)
		fmt.Fprint(&b, "--\n")
	}
	fmt.Fprint(&b, "-- Canonical SQL schema for this resource. Regenerated from proto on\n")
	fmt.Fprint(&b, "-- every `buf generate`. This file is a reference document; it is\n")
	fmt.Fprint(&b, "-- not applied to the database. Your migrations directory holds the\n")
	fmt.Fprint(&b, "-- deploy mechanism — hand-authored forward-only SQL.\n")
	fmt.Fprint(&b, "--\n")
	fmt.Fprint(&b, "-- Authoring a migration: `protoc-gen-go-jet diff` scaffolds an LLM brief\n")
	fmt.Fprint(&b, "-- with a structural DELTA and the concatenated TARGET. The patterns live\n")
	fmt.Fprint(&b, "-- in .claude/skills/protoc-gen-go-jet/SKILL.md. `protoc-gen-go-jet check`\n")
	fmt.Fprint(&b, "-- is the machine-verified guardrail at the end of the flow.\n\n")
	renderCreateTable(&b, p)
	renderRenameDirectives(&b, p)
	renderSupplementalUniques(&b, p)
	renderIndexes(&b, p)
	renderUniqueColumnIndexes(&b, p)
	renderJSONBPathIndexes(&b, p)
	renderJSONBGinIndexes(&b, p)
	renderForeignKeyIndexes(&b, p)
	renderRLS(&b, p)
	renderComments(&b, p)
	renderCustomSQL(&b, p)
	return b.String()
}

// renderComments emits COMMENT ON TABLE / COMMENT ON COLUMN statements
// sourced from proto leading comments. Deterministic order (synthesized
// columns first by name, then proto declaration order) so re-running
// the emitter produces byte-identical ddl.sql.
//
// Proto is already the authoritative source of field docs; surfacing
// that same text in Postgres means psql \d+, pgAdmin, and any ORM that
// reads pg_description get the same documentation the SDK has.
func renderComments(b *strings.Builder, p *ResourcePlan) {
	if p.TableComment == "" && !anyColumnComment(p) && !anyOneofComment(p) {
		return
	}
	b.WriteString("\n")
	if p.TableComment != "" {
		fmt.Fprintf(b, "COMMENT ON TABLE %s IS %s;\n", p.TableName, sqlDollarQuote(p.TableComment))
	}
	for _, c := range orderedColumns(p) {
		if c.Comment == "" {
			continue
		}
		fmt.Fprintf(b, "COMMENT ON COLUMN %s.%s IS %s;\n", p.TableName, c.DBName, sqlDollarQuote(c.Comment))
	}
	// Oneof comments apply to both the discriminator and the JSON
	// value column — a reader looking at either via `\d+` sees the
	// oneof's purpose. Duplicates the string across two catalog
	// entries, which is cheap and worth it for documentation.
	for _, oc := range p.OneofColumns {
		if oc.Comment == "" {
			continue
		}
		q := sqlDollarQuote(oc.Comment)
		fmt.Fprintf(b, "COMMENT ON COLUMN %s.%s IS %s;\n", p.TableName, oc.KindColumn, q)
		fmt.Fprintf(b, "COMMENT ON COLUMN %s.%s IS %s;\n", p.TableName, oc.JSONColumn, q)
	}
}

// anyOneofComment — the gate matches anyColumnComment so
// renderComments only emits a trailing newline + section when
// there's actually something to document.
func anyOneofComment(p *ResourcePlan) bool {
	for _, oc := range p.OneofColumns {
		if oc.Comment != "" {
			return true
		}
	}
	return false
}

func anyColumnComment(p *ResourcePlan) bool {
	for _, c := range p.Columns {
		if c.Comment != "" {
			return true
		}
	}
	return false
}

// sqlDollarQuote wraps s as a Postgres dollar-quoted string literal.
// Picks a tag that does not appear in s (usually the empty tag works)
// so the value never needs single-quote escaping.
func sqlDollarQuote(s string) string {
	tag := ""
	for strings.Contains(s, "$"+tag+"$") {
		tag += "x"
	}
	return "$" + tag + "$" + s + "$" + tag + "$"
}

func renderCreateTable(b *strings.Builder, p *ResourcePlan) {
	fmt.Fprintf(b, "CREATE TABLE %s (\n", p.TableName)

	ordered := orderedColumns(p)

	width := 0
	for _, c := range ordered {
		if len(c.DBName) > width {
			width = len(c.DBName)
		}
	}
	for _, oc := range p.OneofColumns {
		if len(oc.KindColumn) > width {
			width = len(oc.KindColumn)
		}
		if len(oc.JSONColumn) > width {
			width = len(oc.JSONColumn)
		}
	}

	cols := []string{}

	// Column defs
	for _, c := range ordered {
		def := fmt.Sprintf("    %-*s  %s", width, c.DBName, c.SQLType)
		if c.GeneratedExpr != "" {
			// PG syntax: `<type> GENERATED ALWAYS AS (<expr>) STORED [NOT NULL]`.
			// The NOT NULL clause comes after STORED; default_expr is
			// disallowed alongside this branch (resolver rejects the
			// combo). The expression body is emitted verbatim — the
			// resolver has already enforced no `;` to keep this safe
			// inside a CREATE TABLE.
			def += " GENERATED ALWAYS AS (" + c.GeneratedExpr + ") STORED"
			if c.NotNull {
				def += " NOT NULL"
			}
		} else {
			if c.NotNull {
				def += " NOT NULL"
			}
			if c.DefaultExpr != "" {
				def += " DEFAULT " + c.DefaultExpr
			}
		}
		cols = append(cols, def)
	}

	// Oneof pair columns (kind + json). The CHECK constraint on the kind
	// column is emitted as a table-level constraint below. The kind
	// column gets DEFAULT '' only when the oneof is optional — for
	// required oneofs '' is not in the kind CHECK list, so the
	// inferred default would violate it. Callers must set the kind
	// explicitly on insert in that case. The JSONB column always
	// defaults to '{}', a legal JSONB object.
	for _, oc := range p.OneofColumns {
		if oc.Optional {
			cols = append(cols, fmt.Sprintf("    %-*s  TEXT NOT NULL DEFAULT ''", width, oc.KindColumn))
		} else {
			cols = append(cols, fmt.Sprintf("    %-*s  TEXT NOT NULL", width, oc.KindColumn))
		}
		cols = append(cols, fmt.Sprintf("    %-*s  JSONB NOT NULL DEFAULT '{}'::jsonb", width, oc.JSONColumn))
	}

	// Primary key
	if len(p.PrimaryKey) > 0 {
		cols = append(cols, fmt.Sprintf("    PRIMARY KEY (%s)", strings.Join(p.PrimaryKey, ", ")))
	}

	// CHECK constraints (field-level)
	for _, c := range ordered {
		if c.Check == "" {
			continue
		}
		cols = append(cols, fmt.Sprintf("    CONSTRAINT %s_%s_check CHECK (%s)", p.TableName, c.DBName, c.Check))
	}

	// Oneof CHECK constraints
	for _, oc := range p.OneofColumns {
		cols = append(cols, fmt.Sprintf("    CONSTRAINT %s_%s_kind_valid CHECK (%s)", p.TableName, oc.BaseName, oneofKindCheck(oc)))
	}

	// Foreign keys — inlined into CREATE TABLE so the referenced
	// table name is self-describing in the DDL text. The applier
	// topo-sorts ddl.sql files by these REFERENCES clauses before
	// running them, so "table must exist before its referrer" is
	// handled without a two-pass split or sentinel marker.
	for _, c := range ordered {
		fk := c.ForeignKey
		if fk == nil {
			continue
		}
		localCols, targetCols := c.DBName, fk.TargetColumn
		if fk.TenantComposite {
			localCols = fk.LocalTenantColumn + ", " + c.DBName
			targetCols = fk.TargetTenantColumn + ", " + fk.TargetColumn
		}
		cols = append(cols, fmt.Sprintf(
			"    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s",
			fk.ConstraintName, localCols, fk.TargetTable, targetCols, fk.OnDelete, fk.OnUpdate))
	}

	b.WriteString(strings.Join(cols, ",\n"))
	if p.Partition != nil {
		fmt.Fprintf(b, "\n) PARTITION BY %s (%s);\n", p.Partition.Method, strings.Join(p.Partition.Columns, ", "))
	} else {
		b.WriteString("\n);\n")
	}
}

// orderedColumns returns columns in a stable order: synthesized first (so
// tenant_id/user_id sit at the top of the table DDL), then proto fields in
// declaration order. Within synthesized, a stable alphabetic ordering.
func orderedColumns(p *ResourcePlan) []ColumnPlan {
	synth := []ColumnPlan{}
	proto := []ColumnPlan{}
	for _, c := range p.Columns {
		if c.Synthesized {
			synth = append(synth, c)
		} else {
			proto = append(proto, c)
		}
	}
	sort.SliceStable(synth, func(i, j int) bool { return synth[i].DBName < synth[j].DBName })
	return append(synth, proto...)
}

func oneofKindCheck(oc OneofColumnPlan) string {
	values := []string{}
	if oc.Optional {
		values = append(values, "''")
	}
	for _, v := range oc.Variants {
		values = append(values, fmt.Sprintf("'%s'", v.VariantName))
	}
	return fmt.Sprintf("%s IN (%s)", oc.KindColumn, strings.Join(values, ", "))
}

func renderIndexes(b *strings.Builder, p *ResourcePlan) {
	if len(p.Indexes) == 0 {
		return
	}
	b.WriteString("\n")
	for _, idx := range p.Indexes {
		unique := ""
		if idx.Unique {
			unique = "UNIQUE "
		}
		cols := []string{}
		for i, c := range idx.Columns {
			if idx.Order[i] == "DESC" {
				cols = append(cols, c+" DESC")
			} else {
				cols = append(cols, c)
			}
		}
		line := fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", unique, idx.Name, p.TableName, strings.Join(cols, ", "))
		if idx.Where != "" {
			line += " WHERE " + idx.Where
		}
		b.WriteString(line + ";\n")
	}
}

// renderRenameDirectives emits one comment line per column that
// declares `rename_from`. The directive is purely advisory — the
// diff tool (or human author) should translate each one into an
// `ALTER TABLE <t> RENAME COLUMN <old> TO <new>` migration statement
// instead of letting pg-schema-diff emit DROP+ADD.
//
// Format is stable enough that a follow-up iteration can parse these
// directives to pre-align the migrations-side DB before running
// pg-schema-diff; keep the exact prefix in sync with any such parser.
func renderRenameDirectives(b *strings.Builder, p *ResourcePlan) {
	any := false
	for _, c := range p.Columns {
		if c.RenameFrom != "" {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("\n")
	b.WriteString("-- PLUGIN-RENAME directives — declared via `(storage.v1.column).rename_from`.\n")
	b.WriteString("-- Author the next migration as `ALTER TABLE <t> RENAME COLUMN <old> TO <new>`\n")
	b.WriteString("-- for each line below, then drop the annotation on the next release once\n")
	b.WriteString("-- every environment has applied the rename.\n")
	for _, c := range p.Columns {
		if c.RenameFrom == "" {
			continue
		}
		fmt.Fprintf(b, "-- PLUGIN-RENAME: %s %s -> %s\n", p.TableName, c.RenameFrom, c.DBName)
	}
}

// renderUniqueColumnIndexes emits one unique btree index per column
// with `unique: true`. Scope expansion mirrors the RLS boundary so
// constraint violations can never leak another tenant's (or another
// user's) row via the duplicate-key error:
//
//   - tenant-scoped tables  → (tenant_id, col)
//   - user-scoped tables    → (tenant_id, user_id, col)
//   - no tenancy            → (col)
//
// Name pattern: `idx_<table>_<col>_unique`. Explicit `Table.indexes`
// entries win when a caller wants a different name, column set, or
// partial predicate (e.g. a deliberately-global uniqueness window).
func renderUniqueColumnIndexes(b *strings.Builder, p *ResourcePlan) {
	any := false
	for _, c := range p.Columns {
		if c.Unique {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("\n")
	var scopeCols []string
	if p.Tenancy != nil {
		scopeCols = append(scopeCols, p.Tenancy.Column)
		if p.Tenancy.UserScoped {
			scopeCols = append(scopeCols, p.Tenancy.UserColumn)
		}
	}
	for _, c := range p.Columns {
		if !c.Unique {
			continue
		}
		name := fmt.Sprintf("idx_%s_%s_unique", p.TableName, c.DBName)
		cols := []string{}
		for _, s := range scopeCols {
			if s == c.DBName {
				continue // avoid `(tenant_id, tenant_id)` for a unique-on-the-scope-column case
			}
			cols = append(cols, s)
		}
		cols = append(cols, c.DBName)
		fmt.Fprintf(b, "CREATE UNIQUE INDEX %s ON %s (%s);\n", name, p.TableName, strings.Join(cols, ", "))
	}
}

// renderJSONBGinIndexes emits one GIN index per column marked
// `jsonb_gin_index: true`. Uses `jsonb_path_ops` — 3x smaller than
// the default `jsonb_ops` and faster for containment queries, at
// the cost of the `?` / `?&` / `?|` existence operators. Callers
// needing those can hand-author via `Table.custom_sql`.
func renderJSONBGinIndexes(b *strings.Builder, p *ResourcePlan) {
	any := false
	for _, c := range p.Columns {
		if c.JSONBGinIndex {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("\n")
	for _, c := range p.Columns {
		if !c.JSONBGinIndex {
			continue
		}
		name := fmt.Sprintf("idx_%s_%s_gin", p.TableName, c.DBName)
		fmt.Fprintf(b, "CREATE INDEX %s ON %s USING GIN (%s jsonb_path_ops);\n", name, p.TableName, c.DBName)
	}
}

// renderCustomSQL appends caller-supplied statements verbatim, one
// per line. Statements have already been normalised at resolve time
// (trailing semicolon stripped, empty entries dropped) so the
// emitter can add exactly one `;` + newline per entry.
//
// The `-- custom_sql` marker makes the escape-hatch boundary
// visible in the emitted DDL so a migration reviewer notices
// immediately that the following section isn't plugin-managed in
// the usual sense.
func renderCustomSQL(b *strings.Builder, p *ResourcePlan) {
	// Normalise first so we can skip the marker comment entirely when
	// every entry is effectively blank — dropping the dangling
	// `-- custom_sql` header that would otherwise sit above nothing.
	// resolveResource already normalises; this pass is defensive for
	// directly-constructed plans (tests, future tools).
	cleaned := make([]string, 0, len(p.CustomSQL))
	for _, stmt := range p.CustomSQL {
		// Strip both trailing whitespace and semicolons — inputs like
		// `"stmt ;"` or `"stmt;;; "` should all normalise to `stmt;`
		// in the output, not `stmt ;` or `stmt;;`.
		clean := strings.TrimRight(stmt, "; \t\r\n")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			continue
		}
		cleaned = append(cleaned, clean)
	}
	if len(cleaned) == 0 {
		return
	}
	b.WriteString("\n-- custom_sql — caller-supplied escape hatch; keep statements idempotent\n")
	for _, stmt := range cleaned {
		fmt.Fprintf(b, "%s;\n", stmt)
	}
}

// renderJSONBPathIndexes emits one btree expression index per declared
// jsonb_indexed_paths entry. Path "a.b.c" compiles to
// `((col->'a'->'b'->>'c'))`; a single key "a" compiles to
// `((col->>'a'))`. The index name embeds the sanitised path so
// pg_schema_diff can detect drift unambiguously when a path is added
// or removed.
func renderJSONBPathIndexes(b *strings.Builder, p *ResourcePlan) {
	any := false
	for _, c := range p.Columns {
		if len(c.JSONBIndexedPaths) > 0 {
			any = true
			break
		}
	}
	if !any {
		for _, oc := range p.OneofColumns {
			if len(oc.JSONBIndexedPaths) > 0 {
				any = true
				break
			}
		}
	}
	if !any {
		return
	}
	b.WriteString("\n")
	for _, c := range p.Columns {
		for _, path := range c.JSONBIndexedPaths {
			expr := jsonbPathExpr(c.DBName, path)
			name := jsonbPathIndexName(p.TableName, c.DBName, path)
			fmt.Fprintf(b, "CREATE INDEX %s ON %s ((%s));\n", name, p.TableName, expr)
		}
	}
	for _, oc := range p.OneofColumns {
		for _, path := range oc.JSONBIndexedPaths {
			expr := jsonbPathExpr(oc.JSONColumn, path)
			name := jsonbPathIndexName(p.TableName, oc.JSONColumn, path)
			fmt.Fprintf(b, "CREATE INDEX %s ON %s ((%s));\n", name, p.TableName, expr)
		}
	}
}

// jsonbPathExpr builds the Postgres JSON accessor chain. The final
// hop uses ->> so the expression evaluates to text (btree-indexable);
// intermediate hops use -> to keep traversing a JSON object.
func jsonbPathExpr(col, path string) string {
	segs := strings.Split(path, ".")
	if len(segs) == 1 {
		return fmt.Sprintf("%s->>'%s'", col, segs[0])
	}
	var sb strings.Builder
	sb.WriteString(col)
	for i, seg := range segs {
		op := "->"
		if i == len(segs)-1 {
			op = "->>"
		}
		fmt.Fprintf(&sb, "%s'%s'", op, seg)
	}
	return sb.String()
}

// jsonbPathIndexName produces a deterministic, identifier-safe index
// name. Dots become underscores so the name is a single SQL token,
// and the full path participates so two paths on the same column get
// distinct indexes. Postgres folds unquoted identifiers to lowercase,
// so we pre-lowercase the path here — otherwise pg_indexes lookups and
// pg_schema_diff round-trips would see a rename where the caller saw
// none.
func jsonbPathIndexName(table, col, path string) string {
	safe := strings.ReplaceAll(path, ".", "_")
	return strings.ToLower(fmt.Sprintf("idx_%s_%s_%s", table, col, safe))
}

func renderRLS(b *strings.Builder, p *ResourcePlan) {
	if p.Tenancy == nil {
		return
	}
	fmt.Fprintf(b, "\nALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", p.TableName)

	name, pred := "tenant_isolation", tenantPredicate(p.Tenancy)
	if p.Tenancy.UserScoped {
		name = "tenant_user_isolation"
	}
	fmt.Fprintf(b, "CREATE POLICY %s ON %s\n", name, p.TableName)
	fmt.Fprintf(b, "    TO %s\n", pgIdent(p.Tenancy.RuntimeRole))
	fmt.Fprintf(b, "    USING      %s\n", pred)
	fmt.Fprintf(b, "    WITH CHECK %s;\n", pred)
}

// tenantPredicate builds the parenthesised USING / WITH CHECK expression
// for the RLS policy. Tenant-only tables get a single equality against
// app.tenant_id; user-scoped tables AND in app.user_id. Keeping the
// expression in one place stops USING and WITH CHECK from ever drifting
// apart, which would silently allow reads the writer can't make (or the
// other way around).
func tenantPredicate(t *TenancyPlan) string {
	tenantEq := fmt.Sprintf("%s = current_setting('app.tenant_id', true)", t.Column)
	if !t.UserScoped {
		return "(" + tenantEq + ")"
	}
	userEq := fmt.Sprintf("%s = current_setting('app.user_id', true)", t.UserColumn)
	return "(" + tenantEq + "\n                AND " + userEq + ")"
}

// renderSupplementalUniques emits UNIQUE constraints injected onto
// THIS table by the FK resolver to back incoming tenant-composite
// foreign keys. Emitted as CREATE UNIQUE INDEX rather than ALTER
// TABLE ADD CONSTRAINT because the index form matches the drift-
// check snapshot's index listing and keeps the ddl.sql section
// ordering consistent with other generated indexes.
//
// The order is deterministic (SupplementalUniques is appended in
// resolve-pass order, de-duplicated by name) so regenerations
// produce byte-identical output.
func renderSupplementalUniques(b *strings.Builder, p *ResourcePlan) {
	if len(p.SupplementalUniques) == 0 {
		return
	}
	b.WriteString("\n")
	b.WriteString("-- Supplemental UNIQUE constraints backing tenant-composite foreign keys.\n")
	b.WriteString("-- Auto-emitted because the primary key does not already cover these columns.\n")
	for _, uq := range p.SupplementalUniques {
		fmt.Fprintf(b, "CREATE UNIQUE INDEX %s ON %s (%s);\n", uq.Name, p.TableName, strings.Join(uq.Columns, ", "))
	}
}

// renderForeignKeyIndexes emits one btree index per FK column whose
// BackingIndex flag survived auto-suppression. The index name
// (`idx_<table>_<col>_fk`) is distinct from the `_unique` / `_gin` /
// `_kind_valid` suffixes the plugin uses elsewhere so collisions
// surface early at checkSynthIdentLen rather than apply time.
func renderForeignKeyIndexes(b *strings.Builder, p *ResourcePlan) {
	any := false
	for _, c := range p.Columns {
		if c.ForeignKey != nil && c.ForeignKey.BackingIndex {
			any = true
			break
		}
	}
	if !any {
		return
	}
	b.WriteString("\n")
	for _, c := range p.Columns {
		if c.ForeignKey == nil || !c.ForeignKey.BackingIndex {
			continue
		}
		fmt.Fprintf(b, "CREATE INDEX %s ON %s (%s);\n", c.ForeignKey.IndexName, p.TableName, c.DBName)
	}
}

// pgIdent wraps s as a double-quoted Postgres identifier with any
// embedded double-quote escaped to "". Used for identifiers that
// can't be plain lowercase snake_case — RLS role names commonly
// contain hyphens (e.g. "app-tenant") which need quoting.
//
// Go's `fmt %q` is not suitable here: it produces Go-string
// escaping (backslash-quote), which PG doesn't recognise as quote
// escaping. PG's convention is doubled-double-quotes inside a
// double-quoted identifier.
func pgIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}
