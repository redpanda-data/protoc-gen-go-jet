package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// normaliseSQLType translates Postgres information_schema column info
// (data_type + udt_name) into the canonical SQL-type spelling the
// plugin uses in emitted DDL. The drift-check diffs on these strings,
// so a silent mismatch here = false positives / negatives on every
// CI run. Pin each mapping.
func TestNormaliseSQLType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		dataType string
		udtName  string
		want     string
	}{
		{"ARRAY", "_text", "TEXT[]"},
		{"ARRAY", "_bool", "BOOLEAN[]"},
		{"ARRAY", "_int4", "INTEGER[]"},
		{"ARRAY", "_int8", "BIGINT[]"},
		{"ARRAY", "_float4", "REAL[]"},
		{"ARRAY", "_float8", "DOUBLE PRECISION[]"},
		{"ARRAY", "_bytea", "BYTEA[]"},
		{"ARRAY", "_timestamptz", "TIMESTAMPTZ[]"},
		{"text", "text", "TEXT"},
		{"jsonb", "jsonb", "JSONB"},
		{"timestamp with time zone", "timestamptz", "TIMESTAMPTZ"},
		{"boolean", "bool", "BOOLEAN"},
		{"integer", "int4", "INTEGER"},
		{"bigint", "int8", "BIGINT"},
		{"real", "float4", "REAL"},
		{"double precision", "float8", "DOUBLE PRECISION"},
		{"bytea", "bytea", "BYTEA"},
		// Unknown types pass through upper-cased — conservative fallback.
		{"numeric", "numeric", "NUMERIC"},
		// Unknown array types fall through too, so a new kind surfaces
		// as drift rather than sneaking past as a plain "ARRAY".
		{"ARRAY", "_numeric", "ARRAY"},
	}
	for _, tt := range tests {
		// Compose subtest name from data_type + udt so ARRAY-variant
		// cases don't collide under one `ARRAY` name and mask each
		// other in non-verbose output.
		name := tt.dataType
		if tt.udtName != "" && tt.udtName != tt.dataType {
			name += "/" + tt.udtName
		}
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, normaliseSQLType(tt.dataType, tt.udtName))
		})
	}
}

// normaliseDefault strips PG's `::<type>` cast suffix and trims
// whitespace. The migrations-side uses `'{}'::jsonb` (cast-suffixed);
// information_schema returns `'{}'` (bare). Normalising both to the
// un-cast form lets cmp.Diff treat them as equal.
func TestNormaliseDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"'{}'", "'{}'"},
		{"'{}'::jsonb", "'{}'"},
		{"  '{}'::jsonb  ", "'{}'"},
		{"now()", "now()"},
		{"NULL", "NULL"},
		// Only the first :: splits — any further cast stays intact after
		// trimming, which is intentional; DDL-authored defaults shouldn't
		// stack casts anyway.
		{"0::integer::text", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, normaliseDefault(tt.in))
		})
	}
}

// stripOuterParens peels only matched outer parens from a CHECK
// body. `(a)` → `a`, `((a))` → `a`, but `(a) OR (b)` stays intact —
// the outer parens don't span the whole expression.
func TestStripOuterParens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"a", "a"},
		{"(a)", "a"},
		{"((a))", "a"},
		{"(a = b)", "a = b"},
		{"(a) OR (b)", "(a) OR (b)"},
		{"(a AND (b OR c))", "a AND (b OR c)"},
		{"", ""},
		{"()", ""},
		// PG rewrites boolean CHECK bodies with inner grouping
		// parens. pg_get_constraintdef returns shapes like
		// `((a >= 0) AND (b <= 100))` — stripOuterParens peels one
		// matched outer layer, leaving the inner grouping intact.
		// Both sides of the drift check come through the same
		// pg_get_constraintdef pipeline, so the inner form matches
		// on both sides and diff succeeds even though it isn't
		// the plugin's literal input shape.
		{"((a >= 0) AND (b <= 100))", "(a >= 0) AND (b <= 100)"},
		// Edge: unbalanced parens — don't peel. Otherwise `(a` would
		// become `a` and corrupt downstream diffing.
		{"(a AND b", "(a AND b"},
		{"a AND b)", "a AND b)"},
		// Edge: nested-but-unbalanced — the first `)` closes the
		// outer before the end, so the outer parens don't span the
		// whole string; don't peel.
		{"(a) AND (b)", "(a) AND (b)"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, stripOuterParens(tt.in))
		})
	}
}

// normaliseIndexDef drops the "public." schema qualifier and collapses
// runs of whitespace so information_schema's CREATE INDEX string can
// diff cleanly against the hand-authored ddl.sql form. The plugin-
// emitted DDL is unqualified ("CREATE INDEX ... ON things (...)"); PG
// always returns schema-qualified and expanded whitespace.
func TestNormaliseIndexDef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "btree with whitespace",
			in:   "CREATE INDEX idx_things_tenant_id_name ON public.things  USING btree (tenant_id, name)",
			want: "CREATE INDEX idx_things_tenant_id_name ON things USING btree (tenant_id, name)",
		},
		{
			// GIN index shape landed with the jsonb_gin_index feature.
			// PG stores `USING gin (spec jsonb_path_ops)`; the public.
			// schema prefix strips the same way.
			name: "gin jsonb_path_ops",
			in:   "CREATE INDEX idx_things_spec_gin ON public.things USING gin (spec jsonb_path_ops)",
			want: "CREATE INDEX idx_things_spec_gin ON things USING gin (spec jsonb_path_ops)",
		},
		{
			// BRIN indexes (common in custom_sql) strip the same way.
			name: "brin from custom_sql",
			in:   "CREATE INDEX idx_things_created_at_brin ON public.things USING brin (created_at)",
			want: "CREATE INDEX idx_things_created_at_brin ON things USING brin (created_at)",
		},
		{
			// Expression index on a JSONB path — PG adds `::text` casts
			// and extra grouping parens. normaliseIndexDef doesn't
			// reshape those; it only strips the schema prefix and
			// normalises whitespace. Drift check still succeeds because
			// both sides go through pg_get_indexdef and receive the
			// same canonical form.
			name: "jsonb expression index",
			in:   "CREATE INDEX idx_things_spec_api_key ON public.things USING btree (((spec ->> 'api_key'::text)))",
			want: "CREATE INDEX idx_things_spec_api_key ON things USING btree (((spec ->> 'api_key'::text)))",
		},
		{
			// UNIQUE index shape — CREATE UNIQUE INDEX threads through
			// normaliseIndexDef the same way.
			name: "unique index",
			in:   "CREATE UNIQUE INDEX idx_things_name_unique ON public.things USING btree (name)",
			want: "CREATE UNIQUE INDEX idx_things_name_unique ON things USING btree (name)",
		},
		{
			// Partial index with WHERE clause — the schema prefix
			// could also appear inside the WHERE, though pg_indexes
			// rarely emits that form. Defensive coverage.
			name: "partial index",
			in:   "CREATE UNIQUE INDEX idx_things_enabled ON public.things USING btree (enabled, name DESC) WHERE (enabled = true)",
			want: "CREATE UNIQUE INDEX idx_things_enabled ON things USING btree (enabled, name DESC) WHERE (enabled = true)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normaliseIndexDef(tt.in))
		})
	}
}

// extractTableName pulls the table identifier out of a DDL file's
// first CREATE TABLE line. The drift check discovers plugin-managed
// tables this way — wrong parse = wrong diff target.
//
// The parser is intentionally narrow: it expects the plugin-emitted
// shape `CREATE TABLE <name> (` with the opening paren at end of line.
// That's what renderCreateTable produces. Hand-written DDL that puts
// the full column list on the same line would need a real SQL tokeniser;
// we don't need one because plugin output is the only input.
func TestExtractTableName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			// Historical shape — the plugin used to preface ddl.sql
			// with `SET statement_timeout` / `SET lock_timeout`. Kept
			// in the parser-robustness suite so hand-authored
			// migrations (copied from old ddl.sql, or adding their
			// own SET prelude) still parse.
			name: "preamble-tolerant: SET statements before CREATE",
			body: "-- comment\nSET statement_timeout = '30s';\n\nCREATE TABLE things (\n    id TEXT\n);\n",
			want: "things",
		},
		{
			name: "lowercase keyword, end-of-line paren",
			body: "create table widgets (\n    id TEXT\n);\n",
			want: "widgets",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ddl.sql")
			require.NoError(t, os.WriteFile(path, []byte(tt.body), 0o600))
			got, err := extractTableName(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractTableName_NoCreateTable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ddl.sql")
	require.NoError(t, os.WriteFile(path, []byte("-- nothing here\nSELECT 1;\n"), 0o600))
	_, err := extractTableName(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CREATE TABLE line")
}

// TestExtractTableName_ReadError pins that the function returns a
// wrapped error naming the path when the file is unreadable. The
// error message is what an operator sees when a stale ddl.sql
// path slipped into `--ddl-roots`; it has to name the file so the
// fix is obvious.
func TestExtractTableName_ReadError(t *testing.T) {
	t.Parallel()
	_, err := extractTableName(filepath.Join(t.TempDir(), "does-not-exist.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.sql")
	assert.Contains(t, err.Error(), "read")
}

func TestParseRenameDirectives_Basic(t *testing.T) {
	ddl := `
CREATE TABLE things (id TEXT);

-- PLUGIN-RENAME directives — declared via ` + "`" + `(storage.v1.column).rename_from` + "`" + `.
-- Author the next migration as ` + "`" + `ALTER TABLE <t> RENAME COLUMN <old> TO <new>` + "`" + `
-- for each line below, then drop the annotation on the next release once
-- every environment has applied the rename.
-- PLUGIN-RENAME: things old_name -> display_name
-- PLUGIN-RENAME: things status -> state

CREATE INDEX idx_things_state ON things (state);
`
	got := parseRenameDirectives(ddl)
	assert.Equal(t, []renameDirective{
		{Table: "things", From: "old_name", To: "display_name"},
		{Table: "things", From: "status", To: "state"},
	}, got)
}

func TestParseRenameDirectives_IgnoresHeaderAndGarbage(t *testing.T) {
	// The header line ("-- PLUGIN-RENAME directives — declared via...")
	// contains the prefix but no "->" arrow, so it must not be parsed
	// as a directive. Truly malformed lines (missing arrow, wrong arity)
	// are also silently skipped — they'd otherwise turn every
	// hand-edited DDL into a hard error.
	ddl := `
-- PLUGIN-RENAME directives — declared via (storage.v1.column).rename_from.
-- PLUGIN-RENAME: malformed line without arrow
-- PLUGIN-RENAME: too few tokens -> x
-- PLUGIN-RENAME: actually_valid things old -> new
-- PLUGIN-RENAME:   widgets  foo  ->  bar
-- not a plugin rename line
`
	got := parseRenameDirectives(ddl)
	// Only the `widgets foo -> bar` line is well-formed. `actually_valid
	// things old -> new` has three tokens before the arrow (max is two)
	// and is correctly skipped.
	assert.Equal(t, []renameDirective{
		{Table: "widgets", From: "foo", To: "bar"},
	}, got)
}

func TestParseRenameDirectives_None(t *testing.T) {
	ddl := "CREATE TABLE things (id TEXT);\n"
	got := parseRenameDirectives(ddl)
	assert.Nil(t, got)
}

// TestStringSlice_Set exercises the comma-split + whitespace-trim
// + empty-skip behaviour the `--ddl-roots` flag relies on. Shell
// expansions can easily slip an empty entry in (`--ddl-roots=,,x`)
// or trailing whitespace from copy-pasted paths; both must land on
// a sensible slice.
func TestStringSlice_Set(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string // multiple Set() calls — flag package may call it more than once
		want []string
	}{
		{name: "single", in: []string{"one"}, want: []string{"one"}},
		{name: "comma_separated", in: []string{"a,b,c"}, want: []string{"a", "b", "c"}},
		{name: "whitespace_trimmed", in: []string{" a , b , c "}, want: []string{"a", "b", "c"}},
		{name: "empty_skipped", in: []string{",,x,,"}, want: []string{"x"}},
		{name: "repeated_Set", in: []string{"a", "b,c"}, want: []string{"a", "b", "c"}},
		{name: "all_empty", in: []string{",,,"}, want: nil},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s stringSlice
			for _, v := range tt.in {
				require.NoError(t, s.Set(v))
			}
			assert.Equal(t, tt.want, []string(s))
		})
	}
}

// TestStringSlice_String round-trips the slice through its own
// formatter — flag.Value's String() is what `--help` and error
// messages surface; keep it readable.
func TestStringSlice_String(t *testing.T) {
	t.Parallel()
	s := stringSlice{"a", "b", "c"}
	assert.Equal(t, "a,b,c", s.String())
}

// TestDiscoverPluginTables — the drift check discovers plugin-managed
// tables by globbing `*_ddl.sql` under each root. Non-plugin files
// (schema_migrations, token_vault_*) don't get touched. This test
// walks a tempdir layout and confirms the glob + extract + sort
// pipeline produces the expected set.
func TestDiscoverPluginTables(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Plugin-emitted files.
	write := func(name, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("alpha_ddl.sql", "CREATE TABLE alpha (\n  id TEXT\n);\n")
	write("beta_ddl.sql", "CREATE TABLE beta (\n  id TEXT\n);\n")
	write("gamma_ddl.sql", "CREATE TABLE gamma (\n  id TEXT\n);\n")
	// Non-plugin file — must not be picked up.
	write("notes.md", "# ignore me\n")
	write("migration.up.sql", "ALTER TABLE ...")

	got, err := discoverPluginTables([]string{dir}, nil)
	require.NoError(t, err)
	tables := make([]string, 0, len(got))
	for _, p := range got {
		tables = append(tables, p.Table)
	}
	// Output must be alphabetically sorted — stable order keeps the
	// diff output deterministic.
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, tables)
}

// TestDiscoverPluginTables_MissingRoot ensures an unreadable root
// doesn't panic — an empty glob surfaces as zero matches, not an
// error.
func TestDiscoverPluginTables_MissingRoot(t *testing.T) {
	t.Parallel()
	got, err := discoverPluginTables([]string{filepath.Join(t.TempDir(), "does-not-exist")}, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestDiscoverPluginTables_ResourceFilter pins the --resources filter:
// two ddl.sql files in one dir, --resources lists only one, the other
// is dropped.
func TestDiscoverPluginTables_ResourceFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("alpha_ddl.sql",
		"-- Code generated from alpha.proto by protoc-gen-go-jet. DO NOT EDIT.\n"+
			"--\n"+
			"-- Resource: example.v1.Alpha\n"+
			"--\n"+
			"CREATE TABLE alpha (\n  id TEXT\n);\n")
	write("beta_ddl.sql",
		"-- Code generated from beta.proto by protoc-gen-go-jet. DO NOT EDIT.\n"+
			"--\n"+
			"-- Resource: example.v1.Beta\n"+
			"--\n"+
			"CREATE TABLE beta (\n  id TEXT\n);\n")

	got, err := discoverPluginTables([]string{dir}, []string{"example.v1.Alpha"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "alpha", got[0].Table)
	assert.Equal(t, "example.v1.Alpha", got[0].ResourceFQN)

	// Typo catcher: an FQN with no matching ddl.sql is a hard error.
	_, err = discoverPluginTables([]string{dir}, []string{"example.v1.Alpha", "example.v1.Ghost"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "example.v1.Ghost")
}
