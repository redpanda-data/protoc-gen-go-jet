package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeTableDelta pins the +/-/~ classification and name ordering
// of the structural delta. Uses the snapshot types directly so a
// refactor that breaks struct equality (e.g. adds a non-comparable
// field) fails here before it hits drift-check.
func TestComputeTableDelta(t *testing.T) {
	t.Parallel()

	col := func(name, typ, def, comment string, notNull bool) Column {
		return Column{Name: name, SQLType: typ, NotNull: notNull, DefaultExpr: def, Comment: comment}
	}

	t.Run("identical tables produce empty delta", func(t *testing.T) {
		snap := TableSnapshot{
			Columns:         []Column{col("id", "TEXT", "", "", true), col("name", "TEXT", "''", "", true)},
			PrimaryKey:      []string{"id"},
			RLSEnabled:      true,
			CheckConstraint: []CheckConstraint{{Name: "c1", Body: "name <> ''"}},
		}
		assert.Empty(t, computeTableDelta("t", snap, snap))
	})

	t.Run("added column emits + line", func(t *testing.T) {
		cur := TableSnapshot{Columns: []Column{col("id", "TEXT", "", "", true)}}
		tgt := TableSnapshot{Columns: []Column{
			col("id", "TEXT", "", "", true),
			col("labels", "JSONB", "'{}'", "opaque tags", true),
		}}
		assert.Equal(t, []string{"+ column t.labels"}, computeTableDelta("t", cur, tgt))
	})

	t.Run("dropped column emits - line", func(t *testing.T) {
		cur := TableSnapshot{Columns: []Column{col("id", "TEXT", "", "", true), col("legacy", "TEXT", "", "", false)}}
		tgt := TableSnapshot{Columns: []Column{col("id", "TEXT", "", "", true)}}
		assert.Equal(t, []string{"- column t.legacy"}, computeTableDelta("t", cur, tgt))
	})

	t.Run("type change emits ~ line", func(t *testing.T) {
		cur := TableSnapshot{Columns: []Column{col("n", "INTEGER", "0", "", true)}}
		tgt := TableSnapshot{Columns: []Column{col("n", "BIGINT", "0", "", true)}}
		assert.Equal(t, []string{"~ column t.n"}, computeTableDelta("t", cur, tgt))
	})

	// Regression guard: earlier implementation built a signature string
	// with a bare `|` separator. A column whose comment contains `|`
	// could collide with another column's signature. Struct equality
	// (the current impl) handles this correctly.
	t.Run("pipe in comment does not false-positive", func(t *testing.T) {
		snap := TableSnapshot{Columns: []Column{col("c", "TEXT", "", "a|b|c", false)}}
		assert.Empty(t, computeTableDelta("t", snap, snap))
	})

	t.Run("rename shows up as -old +new pair (mechanical, not reconciled)", func(t *testing.T) {
		cur := TableSnapshot{Columns: []Column{col("description", "TEXT", "''", "", true)}}
		tgt := TableSnapshot{Columns: []Column{col("summary", "TEXT", "''", "", true)}}
		// SKILL.md tells the author to reconcile with Active
		// PLUGIN-RENAME directives; the generator stays mechanical.
		assert.Equal(t, []string{
			"- column t.description",
			"+ column t.summary",
		}, computeTableDelta("t", cur, tgt))
	})

	t.Run("rls / primary_key / table_comment flips", func(t *testing.T) {
		cur := TableSnapshot{PrimaryKey: []string{"id"}, RLSEnabled: false, TableComment: "old"}
		tgt := TableSnapshot{PrimaryKey: []string{"tenant_id", "id"}, RLSEnabled: true, TableComment: "new"}
		assert.Equal(t, []string{
			"~ primary_key t",
			"~ rls t",
			"~ table_comment t",
		}, computeTableDelta("t", cur, tgt))
	})

	t.Run("multiple kinds sort within their own bucket", func(t *testing.T) {
		cur := TableSnapshot{
			Columns:         []Column{col("a", "TEXT", "", "", false)},
			CheckConstraint: []CheckConstraint{{Name: "z_check", Body: "a <> ''"}},
		}
		tgt := TableSnapshot{
			Columns:         []Column{col("a", "TEXT", "", "", false), col("b", "TEXT", "", "", false)},
			CheckConstraint: []CheckConstraint{{Name: "z_check", Body: "a <> ''"}, {Name: "a_check", Body: "b <> ''"}},
		}
		// Column bucket first, then check; each name-sorted internally.
		assert.Equal(t, []string{
			"+ column t.b",
			"+ check t.a_check",
		}, computeTableDelta("t", cur, tgt))
	})
}

// TestStripDDLHeader pins the leading-comment-strip behaviour for
// initial migrations. The current contract is "everything up to the
// first non-comment, non-blank line gets dropped" — reviewer noted
// that intentional advisory comments added before CREATE TABLE in a
// ddl.sql would also be silently dropped. That's a known tradeoff;
// this test locks the contract in.
func TestStripDDLHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "drops DO NOT EDIT preamble",
			in:   "-- Code generated. DO NOT EDIT.\n--\n-- More.\n\nCREATE TABLE t ();\n",
			want: "CREATE TABLE t ();\n",
		},
		{
			name: "no header is pass-through",
			in:   "CREATE TABLE t ();\n",
			want: "CREATE TABLE t ();\n",
		},
		{
			name: "empty input produces empty output",
			in:   "",
			want: "",
		},
		{
			name: "comment-only input strips to empty",
			in:   "-- only comments\n-- nothing else\n",
			want: "",
		},
		{
			name: "preserves body comments (first non-comment seen)",
			in:   "-- header\n\nCREATE TABLE t (\n    -- body comment kept\n    id TEXT\n);\n",
			want: "CREATE TABLE t (\n    -- body comment kept\n    id TEXT\n);\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripDDLHeader(tc.in))
		})
	}
}

// TestHasNonEmptyMigrations checks the empty-scaffold filter the diff
// subcommand uses to tell "first-ever migration" (initial mode) apart
// from "existing migrations, generate a brief" (brief mode). The
// scaffold `migrate create` leaves an empty .up.sql which must not
// flip the mode.
func TestHasNonEmptyMigrations(t *testing.T) {
	t.Parallel()

	t.Run("missing directory returns false, no error", func(t *testing.T) {
		got, err := hasNonEmptyMigrations(filepath.Join(t.TempDir(), "no-such-dir"))
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("empty directory returns false", func(t *testing.T) {
		got, err := hasNonEmptyMigrations(t.TempDir())
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("only empty .up.sql files returns false", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_init.up.sql"), nil, 0o600))
		got, err := hasNonEmptyMigrations(dir)
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("non-empty .up.sql returns true", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_init.up.sql"), []byte("CREATE TABLE t ();\n"), 0o600))
		got, err := hasNonEmptyMigrations(dir)
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("ignores non-.up.sql files", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("noise"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_init.up.sql"), nil, 0o600))
		got, err := hasNonEmptyMigrations(dir)
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("mix of empty and non-empty returns true", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001_init.up.sql"), []byte("CREATE TABLE t ();\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0002_next.up.sql"), nil, 0o600))
		got, err := hasNonEmptyMigrations(dir)
		require.NoError(t, err)
		assert.True(t, got)
	})
}

func TestSQLDollarQuote_CollisionPicksUniqueTag(t *testing.T) {
	t.Parallel()
	// Plain body uses empty tag.
	assert.Equal(t, "$$hello$$", sqlDollarQuote("hello"))
	// Body containing `$$` forces a tag.
	assert.Equal(t, "$x$hello$$there$x$", sqlDollarQuote("hello$$there"))
	// Body containing `$x$` escalates further.
	assert.Equal(t, "$xx$hello$$and$x$$xx$", sqlDollarQuote("hello$$and$x$"))
}

// Regression guard on <Prefix>UpdateAll's column list. A bug here is
// silent — omitted columns get dropped from UPDATE statements, so
// callbacks that set them appear to succeed but the DB keeps the
// old value. These cases encode the exclusion rules so a refactor
// can't accidentally loosen them.
func TestMutableJetFields(t *testing.T) {
	t.Parallel()

	col := func(db string, synth, immutable bool) ColumnPlan {
		return ColumnPlan{DBName: db, JetFieldName: db, Synthesized: synth, Immutable: immutable}
	}

	t.Run("excludes primary-key members", func(t *testing.T) {
		p := &ResourcePlan{
			PrimaryKey: []string{"tenant_id", "name"},
			Columns: []ColumnPlan{
				col("tenant_id", true, false),
				col("name", false, false),
				col("display_name", false, false),
			},
		}
		assert.Equal(t, []string{"display_name"}, mutableJetFields(p))
	})

	t.Run("excludes synthesized columns", func(t *testing.T) {
		p := &ResourcePlan{
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				col("id", false, false),
				col("tenant_id", true, false), // synthesized — always skipped
				col("value", false, false),
			},
		}
		assert.Equal(t, []string{"value"}, mutableJetFields(p))
	})

	t.Run("excludes immutable columns", func(t *testing.T) {
		p := &ResourcePlan{
			PrimaryKey: []string{"name"},
			Columns: []ColumnPlan{
				col("name", false, true), // also immutable
				col("type", false, true), // immutable, not PK
				col("display_name", false, false),
				col("created_at", false, true),
			},
		}
		assert.Equal(t, []string{"display_name"}, mutableJetFields(p))
	})

	t.Run("includes oneof kind and json pair", func(t *testing.T) {
		p := &ResourcePlan{
			PrimaryKey: []string{"name"},
			Columns: []ColumnPlan{
				col("name", false, true),
				col("display_name", false, false),
			},
			OneofColumns: []OneofColumnPlan{
				{
					BaseName:     "provider_config",
					JetKindField: "ProviderConfigKind",
					JetJSONField: "ProviderConfig",
				},
			},
		}
		assert.Equal(t, []string{
			"display_name",
			"ProviderConfigKind",
			"ProviderConfig",
		}, mutableJetFields(p))
	})

	t.Run("updated_at participates when annotated mutable", func(t *testing.T) {
		// created_at is immutable, updated_at is not — updates always
		// need to rewrite updated_at.
		p := &ResourcePlan{
			PrimaryKey: []string{"name"},
			Columns: []ColumnPlan{
				col("name", false, true),
				col("created_at", false, true),
				col("updated_at", false, false),
			},
		}
		assert.Equal(t, []string{"updated_at"}, mutableJetFields(p))
	})
}
