package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderMigration_Basic exercises the SQL rendering with a
// hand-constructed plan. No proto descriptors needed — we only verify
// the SQL template is correct, not the plan-building from proto.
func TestRenderMigration_Basic(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"tenant_id", "name"},
		Tenancy: &TenancyPlan{
			Column:      "tenant_id",
			RuntimeRole: "app-tenant",
		},
		Columns: []ColumnPlan{
			// Synthesized tenant_id (added by resolveResource in real
			// runs; we include it here explicitly for the unit test).
			{DBName: "tenant_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID", JetGoType: "string"},
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name", JetGoType: "string", Check: "name <> ''"},
			{DBName: "enabled", SQLType: "BOOLEAN", NotNull: true, DefaultExpr: "false", Kind: KindScalar, JetFieldName: "Enabled", JetGoType: "bool"},
		},
		Indexes: []IndexPlan{
			{Name: "idx_things_tenant_id_name", Columns: []string{"tenant_id", "name"}, Order: []string{"ASC", "ASC"}},
		},
	}

	got := renderSchemaSQL(p)

	// Preamble
	assert.Contains(t, got, "fixture.proto")
	// SET statement_timeout / lock_timeout deliberately absent —
	// they're apply-time session knobs, not schema state, and
	// keeping them out aligns ddl.sql with what pg-schema-diff
	// can round-trip through its schema model.
	assert.NotContains(t, got, "statement_timeout")
	assert.NotContains(t, got, "lock_timeout")

	// Table + columns
	assert.Contains(t, got, "CREATE TABLE things (")
	assert.Contains(t, got, "tenant_id")
	assert.Contains(t, got, "TEXT NOT NULL")
	assert.Contains(t, got, "name")
	assert.Contains(t, got, "DEFAULT ''")
	assert.Contains(t, got, "BOOLEAN NOT NULL DEFAULT false")

	// PK
	assert.Contains(t, got, "PRIMARY KEY (tenant_id, name)")

	// CHECK
	assert.Contains(t, got, "CONSTRAINT things_name_check CHECK (name <> '')")

	// Index
	assert.Contains(t, got, "CREATE INDEX idx_things_tenant_id_name ON things (tenant_id, name)")

	// RLS
	assert.Contains(t, got, "ENABLE ROW LEVEL SECURITY")
	assert.Contains(t, got, `TO "app-tenant"`)
	assert.Contains(t, got, "current_setting('app.tenant_id'")

	// Order matters: synthesized tenant_id comes first.
	lines := strings.Split(got, "\n")
	var tenantLine, nameLine int
	for i, line := range lines {
		if strings.Contains(line, "tenant_id   ") || strings.Contains(line, "tenant_id  TEXT") {
			if tenantLine == 0 {
				tenantLine = i
			}
		}
		if strings.Contains(line, "name") && strings.Contains(line, "TEXT") && !strings.Contains(line, "tenant_id") {
			if nameLine == 0 {
				nameLine = i
			}
		}
	}
	assert.Greater(t, nameLine, tenantLine, "tenant_id should appear before name (synthesized first)")
}

func TestRenderMigration_Oneof(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "widgets",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name", JetGoType: "string"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "config",
				KindColumn:   "config_kind",
				JSONColumn:   "config",
				JetKindField: "ConfigKind",
				JetJSONField: "Config",
				Variants: []OneofVariant{
					{VariantName: "a_config"},
					{VariantName: "b_config"},
				},
			},
		},
	}
	got := renderSchemaSQL(p)

	// Pair of columns. Required oneof (Optional == false): the kind
	// column has no DEFAULT — '' is not in the kind CHECK list, so an
	// inferred default would violate it. Callers must set the kind
	// explicitly on insert. The JSONB column always defaults to '{}'.
	assert.Regexp(t, `config_kind\s+TEXT NOT NULL,`, got,
		"required oneof kind column must not have a DEFAULT clause")
	assert.NotContains(t, got, "config_kind  TEXT NOT NULL DEFAULT")
	assert.Regexp(t, `config\s+JSONB NOT NULL DEFAULT '\{\}'::jsonb`, got)

	// CHECK with both variants
	assert.Contains(t, got, "CONSTRAINT widgets_config_kind_valid CHECK (config_kind IN ('a_config', 'b_config'))")
}

func TestRenderMigration_OptionalOneof(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "widgets",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "config",
				KindColumn:   "config_kind",
				JSONColumn:   "config",
				JetKindField: "ConfigKind",
				JetJSONField: "Config",
				Optional:     true,
				Variants: []OneofVariant{
					{VariantName: "a_config"},
				},
			},
		},
	}
	got := renderSchemaSQL(p)
	// Optional: the empty string is a permitted discriminator and a
	// valid default — the kind CHECK admits ''.
	assert.Contains(t, got, "CHECK (config_kind IN ('', 'a_config'))")
	assert.Regexp(t, `config_kind\s+TEXT NOT NULL DEFAULT ''`, got,
		"optional oneof kind column must keep DEFAULT ''")
}

func TestRenderMigration_UserScopedTenancy(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "tokens",
		PrimaryKey: []string{"tenant_id", "user_id", "name"},
		Tenancy: &TenancyPlan{
			Column:      "tenant_id",
			RuntimeRole: "app-tenant",
			UserScoped:  true,
			UserColumn:  "user_id",
		},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
			{DBName: "user_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true, JetFieldName: "UserID"},
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE POLICY tenant_user_isolation")
	assert.Contains(t, got, "tenant_id = current_setting('app.tenant_id', true)")
	assert.Contains(t, got, "user_id = current_setting('app.user_id', true)")
}

// TestTenantPredicate pins the RLS predicate shape for the two
// tenancy modes. USING and WITH CHECK inside renderRLS share the
// string this function returns — if these two outputs ever diverge
// by a character, either RLS policy half could admit rows the other
// rejects. That's the auth-leak shape we never want to regress.
func TestTenantPredicate(t *testing.T) {
	t.Parallel()
	tenantOnly := tenantPredicate(&TenancyPlan{Column: "tenant_id"})
	assert.Equal(t, "(tenant_id = current_setting('app.tenant_id', true))", tenantOnly)

	userScoped := tenantPredicate(&TenancyPlan{
		Column:     "tenant_id",
		UserScoped: true,
		UserColumn: "user_id",
	})
	// Keep the expected newline + indentation exact — the DDL aligns the
	// AND clause under the opening paren, which psql \d+ renders nicely.
	wantUS := "(tenant_id = current_setting('app.tenant_id', true)\n                AND user_id = current_setting('app.user_id', true))"
	assert.Equal(t, wantUS, userScoped)
}

// TestRenderRLS_USINGEqualsWithCheck is the invariant the shared
// predicate exists to guarantee: whatever USING permits a SELECT on,
// WITH CHECK must permit the same INSERT/UPDATE on, character for
// character. A character-level mismatch would let a writer succeed
// on a row they can't read back, or the other way around.
func TestRenderRLS_USINGEqualsWithCheck(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		tp   *TenancyPlan
	}{
		{"tenant-only", &TenancyPlan{Column: "tenant_id", RuntimeRole: "r"}},
		{"user-scoped", &TenancyPlan{Column: "tenant_id", RuntimeRole: "r", UserScoped: true, UserColumn: "user_id"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			renderRLS(&b, &ResourcePlan{TableName: "t", Tenancy: tt.tp})
			out := b.String()
			using := extractPred(t, out, "USING      ")
			withCheck := extractPred(t, out, "WITH CHECK ")
			assert.Equal(t, using, withCheck, "USING and WITH CHECK must be identical")
		})
	}
}

// extractPred pulls the parenthesised predicate that follows a given
// prefix ("USING      " or "WITH CHECK ") in the rendered policy,
// balancing parens so the multi-line user-scoped variant is captured
// whole. Used only by the USING/WITH CHECK equality test.
func extractPred(t *testing.T, s, prefix string) string {
	t.Helper()
	i := strings.Index(s, prefix)
	if i < 0 {
		t.Fatalf("prefix %q not found in:\n%s", prefix, s)
	}
	rest := s[i+len(prefix):]
	if !strings.HasPrefix(rest, "(") {
		t.Fatalf("expected ( after %q, got: %q", prefix, rest[:20])
	}
	depth := 0
	for j, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[:j+1]
			}
		}
	}
	t.Fatalf("unbalanced parens after %q", prefix)
	return ""
}

// TestOrderedColumns pins the stable-order contract: synthesized
// columns bubble to the top of every emitted CREATE TABLE (so
// tenant_id / user_id always lead) and sort alphabetically amongst
// themselves; proto-declared columns follow in declaration order.
// Without this ordering, generated DDL would shuffle columns across
// runs whenever proto fields were reordered in the proto file, and
// drift-check would see spurious diffs.
func TestOrderedColumns(t *testing.T) {
	t.Parallel()
	// user_id first in Columns (declaration order), but alphabetic
	// sort should put tenant_id ahead of user_id amongst synthesized.
	p := &ResourcePlan{
		Columns: []ColumnPlan{
			{DBName: "user_id", Synthesized: true},
			{DBName: "display_name"},
			{DBName: "tenant_id", Synthesized: true},
			{DBName: "name"},
			{DBName: "created_at"},
		},
	}
	got := orderedColumns(p)
	var names []string
	for _, c := range got {
		names = append(names, c.DBName)
	}
	assert.Equal(t, []string{
		// synthesized first, alpha-sorted
		"tenant_id", "user_id",
		// proto fields in declaration order
		"display_name", "name", "created_at",
	}, names)
}

// TestOneofKindCheck pins the SQL CHECK expression for the oneof
// discriminator. Two shapes: required (every variant must be set —
// blank string is not a legal value) and optional (blank string is
// admitted as "no variant chosen").
func TestOneofKindCheck(t *testing.T) {
	t.Parallel()
	oc := OneofColumnPlan{
		KindColumn: "provider_config_kind",
		Variants: []OneofVariant{
			{VariantName: "openai_config"},
			{VariantName: "anthropic_config"},
		},
	}
	assert.Equal(t,
		"provider_config_kind IN ('openai_config', 'anthropic_config')",
		oneofKindCheck(oc),
		"required oneof: blank not admitted")

	oc.Optional = true
	assert.Equal(t,
		"provider_config_kind IN ('', 'openai_config', 'anthropic_config')",
		oneofKindCheck(oc),
		"optional oneof: blank prepended")
}

// TestRenderMigration_OneofWidthDominatesTable pins column-width
// alignment when the oneof's kind column name is longer than every
// proto-declared column. Without that contribution, the CREATE TABLE
// body would be printf'd with the scalar width and the oneof pair's
// kind + json columns would sit at a different column — producing
// ragged DDL and breaking any naive diff-based drift check.
//
// The real invariant: the space between the column identifier and
// its type annotation is identical on every line.
func TestRenderMigration_OneofWidthDominatesTable(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "widgets",
		PrimaryKey: []string{"n"},
		Columns: []ColumnPlan{
			{DBName: "n", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "N"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "extraordinarily_long_config",
				KindColumn:   "extraordinarily_long_config_kind",
				JSONColumn:   "extraordinarily_long_config",
				JetKindField: "ExtraordinarilyLongConfigKind",
				JetJSONField: "ExtraordinarilyLongConfig",
				Variants:     []OneofVariant{{VariantName: "one"}},
			},
		},
	}
	got := renderSchemaSQL(p)

	// The widest identifier is `extraordinarily_long_config_kind` (32
	// chars). Every column definition line must pad its identifier to
	// at least that width before the SQL type — verify the short `n`
	// column, the wide kind column, and the wide json column all start
	// their SQL type at the same byte offset.
	//
	// typeOffset walks past the identifier and its padding run and
	// returns the position of the SQL type on the line. The first
	// non-space after the leading-spaces+identifier+padding-spaces
	// sequence is the type.
	lines := strings.Split(got, "\n")
	typeOffset := func(line string) int {
		if !strings.HasPrefix(line, "    ") {
			return -1
		}
		// Skip leading 4-space indent + identifier (runs until a space).
		i := 4
		for i < len(line) && line[i] != ' ' {
			i++
		}
		// Skip padding run.
		for i < len(line) && line[i] == ' ' {
			i++
		}
		return i
	}
	var nOffset, kindOffset, jsonOffset int
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trimmed, "n "):
			nOffset = typeOffset(line)
		case strings.HasPrefix(trimmed, "extraordinarily_long_config_kind "):
			kindOffset = typeOffset(line)
		case strings.HasPrefix(trimmed, "extraordinarily_long_config ") && !strings.Contains(trimmed, "_kind"):
			jsonOffset = typeOffset(line)
		}
	}
	assert.Positive(t, nOffset, "n line not located")
	assert.Equal(t, nOffset, kindOffset, "kind column must align with widest identifier")
	assert.Equal(t, nOffset, jsonOffset, "json column must align with widest identifier")
}

// TestRenderMigration_TwoOneofs pins that a message carrying two
// persisted oneofs emits two independent column pairs and two CHECK
// constraints in declaration order. Without per-oneof isolation the
// second oneof's constraint name would collide with the first, or the
// column order would shuffle across regens and cause spurious diffs.
func TestRenderMigration_TwoOneofs(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "widgets",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "auth",
				KindColumn:   "auth_kind",
				JSONColumn:   "auth",
				JetKindField: "AuthKind",
				JetJSONField: "Auth",
				Variants:     []OneofVariant{{VariantName: "bearer"}, {VariantName: "basic"}},
			},
			{
				BaseName:     "payload",
				KindColumn:   "payload_kind",
				JSONColumn:   "payload",
				JetKindField: "PayloadKind",
				JetJSONField: "Payload",
				Variants:     []OneofVariant{{VariantName: "small"}, {VariantName: "large"}},
			},
		},
	}
	got := renderSchemaSQL(p)

	// Both pairs present.
	assert.Contains(t, got, "auth_kind")
	assert.Contains(t, got, "payload_kind")
	assert.Regexp(t, `auth\s+JSONB NOT NULL DEFAULT '\{\}'::jsonb`, got)
	assert.Regexp(t, `payload\s+JSONB NOT NULL DEFAULT '\{\}'::jsonb`, got)

	// Both CHECK constraints with distinct names.
	assert.Contains(t, got, "CONSTRAINT widgets_auth_kind_valid CHECK (auth_kind IN ('bearer', 'basic'))")
	assert.Contains(t, got, "CONSTRAINT widgets_payload_kind_valid CHECK (payload_kind IN ('small', 'large'))")

	// Declaration order: auth block before payload block. Anchor on
	// the CHECK lines because that's where the emitter proves it walked
	// the OneofColumns slice in order.
	authCheck := strings.Index(got, "widgets_auth_kind_valid")
	payloadCheck := strings.Index(got, "widgets_payload_kind_valid")
	require.Positive(t, authCheck)
	require.Positive(t, payloadCheck)
	assert.Less(t, authCheck, payloadCheck, "declaration order must be preserved: auth before payload")

	// Same ordering for the column pair emissions.
	authCol := strings.Index(got, "auth_kind ")
	payloadCol := strings.Index(got, "payload_kind ")
	assert.Less(t, authCol, payloadCol, "column emission must follow declaration order")
}

// TestOneofKindCheck_SingleVariant pins the minimum-legal oneof shape:
// a single variant renders a one-element IN list and the DDL still
// parses. resolveOneof rejects zero-variant oneofs at plan-build time
// (returning "oneof has no variants"), so the emitter never sees an
// empty Variants slice on a real plan. This test stands as the
// boundary: one variant is legal, anything less is caught upstream.
//
// Don't add a guard in oneofKindCheck for an empty slice — the plan
// is already trusted to be valid by the time the emitter runs, and
// a second validation layer would just be dead code that gives a
// false sense of defence.
func TestOneofKindCheck_SingleVariant(t *testing.T) {
	t.Parallel()
	oc := OneofColumnPlan{
		KindColumn: "x_kind",
		Variants:   []OneofVariant{{VariantName: "only"}},
	}
	assert.Equal(t, "x_kind IN ('only')", oneofKindCheck(oc))

	oc.Optional = true
	assert.Equal(t, "x_kind IN ('', 'only')", oneofKindCheck(oc))
}

func TestRenderMigration_CommentsTableAndColumn(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:    "fixture.proto",
		TableName:    "things",
		TableComment: "the things table",
		PrimaryKey:   []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name", Comment: "the name"},
			{DBName: "value", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Value"},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "COMMENT ON TABLE things IS $$the things table$$;")
	assert.Contains(t, got, "COMMENT ON COLUMN things.name IS $$the name$$;")
	// Column with no Comment must not emit a COMMENT ON.
	assert.NotContains(t, got, "COMMENT ON COLUMN things.value")
}

// TestRenderMigration_CommentsSkippedWhenNone guards against noise —
// resources with no leading proto comments must not emit empty
// COMMENT ON lines or blank sections.
func TestRenderMigration_CommentsSkippedWhenNone(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
		},
	}
	got := renderSchemaSQL(p)
	assert.NotContains(t, got, "COMMENT ON")
}

// TestRenderMigration_CommentsDollarQuoteCollision confirms the
// emitter picks a unique dollar-quote tag when the comment body
// contains `$$`. Without this, the SQL would close the literal early
// and fail to parse.
func TestRenderMigration_CommentsDollarQuoteCollision(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:    "fixture.proto",
		TableName:    "things",
		TableComment: "contains $$ literal",
		PrimaryKey:   []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
		},
	}
	got := renderSchemaSQL(p)
	// Tag escalates to `$x$` because `$$` appears in the body.
	assert.Contains(t, got, "COMMENT ON TABLE things IS $x$contains $$ literal$x$;")
}

func TestRenderMigration_JSONBColumn(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
			{DBName: "metadata", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb", Kind: KindJSONBProto, JetFieldName: "Metadata", JetGoType: "string"},
			{DBName: "tags", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb", Kind: KindJSONBStrMap, JetFieldName: "Tags", JetGoType: "string"},
		},
	}
	got := renderSchemaSQL(p)
	assert.Regexp(t, `metadata\s+JSONB NOT NULL DEFAULT '\{\}'::jsonb`, got)
	assert.Regexp(t, `tags\s+JSONB NOT NULL DEFAULT '\{\}'::jsonb`, got)
}

// TestRenderMigration_RepeatedText pins the TEXT[] rendering for
// repeated string / repeated enum fields. pq.StringArray is the jet
// type; SQL level it's just TEXT[] with a '{}' default.
func TestRenderMigration_RepeatedText(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
			{DBName: "scopes", SQLType: "TEXT[]", NotNull: true, DefaultExpr: "'{}'", Kind: KindRepeatedText, JetFieldName: "Scopes"},
		},
	}
	got := renderSchemaSQL(p)
	assert.Regexp(t, `scopes\s+TEXT\[\] NOT NULL DEFAULT '\{\}'`, got)
}

func TestRenderMigration_UniquePartialIndex(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"name"},
		Columns: []ColumnPlan{
			{DBName: "name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "Name"},
			{DBName: "enabled", SQLType: "BOOLEAN", NotNull: true, DefaultExpr: "false", Kind: KindScalar, JetFieldName: "Enabled"},
		},
		Indexes: []IndexPlan{
			{
				Name: "idx_things_enabled_name", Columns: []string{"enabled", "name"},
				Order: []string{"ASC", "DESC"}, Unique: true, Where: "enabled = true",
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE UNIQUE INDEX idx_things_enabled_name ON things (enabled, name DESC) WHERE enabled = true;")
}

func TestRenderMigration_JSONBPathIndex(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
			{
				DBName: "config", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBProto, JetFieldName: "Config",
				JSONBIndexedPaths: []string{"api_key_ref", "nested.timeout"},
			},
			{
				DBName: "attrs", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBStrMap, JetFieldName: "Attrs",
				JSONBIndexedPaths: []string{"region"},
			},
		},
	}
	got := renderSchemaSQL(p)
	// Top-level key: ->> only.
	assert.Contains(t, got, "CREATE INDEX idx_things_config_api_key_ref ON things ((config->>'api_key_ref'));")
	// Dotted path: chained -> with ->> on the final hop.
	assert.Contains(t, got, "CREATE INDEX idx_things_config_nested_timeout ON things ((config->'nested'->>'timeout'));")
	// Same pattern works on a string-map-backed JSONB column.
	assert.Contains(t, got, "CREATE INDEX idx_things_attrs_region ON things ((attrs->>'region'));")
}

func TestRenderMigration_RenameDirective(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
			{DBName: "display_name", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "DisplayName", RenameFrom: "name"},
			{DBName: "state", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "State", RenameFrom: "status"},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "PLUGIN-RENAME: things name -> display_name")
	assert.Contains(t, got, "PLUGIN-RENAME: things status -> state")
	assert.Contains(t, got, "Author the next migration as `ALTER TABLE <t> RENAME COLUMN")
}

func TestRenderMigration_NoRenameDirectiveWithoutAnnotation(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
		},
	}
	got := renderSchemaSQL(p)
	assert.NotContains(t, got, "PLUGIN-RENAME")
}

// TestRenderMigration_JSONBGinIndex pins the GIN-index SQL shape.
// jsonb_path_ops is non-default but smaller and faster for
// containment queries — the choice matters for production sizing.
func TestRenderMigration_JSONBGinIndex(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
			{
				DBName: "config", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBProto, JetFieldName: "Config",
				JSONBGinIndex: true,
			},
			{
				// A GIN-indexed map column alongside a nested-message
				// one to prove the emitter doesn't care which JSONB
				// variant the column holds.
				DBName: "attrs", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBStrMap, JetFieldName: "Attrs",
				JSONBGinIndex: true,
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE INDEX idx_things_config_gin ON things USING GIN (config jsonb_path_ops);")
	assert.Contains(t, got, "CREATE INDEX idx_things_attrs_gin ON things USING GIN (attrs jsonb_path_ops);")
}

// TestRenderMigration_UniqueColumnShorthand pins the UNIQUE-index
// shape for the column-option shorthand. Non-tenant tables get a
// single-column unique index; tenant-scoped tables prepend tenant_id
// so uniqueness scopes within a tenant rather than globally.
func TestRenderMigration_UniqueColumnShorthand(t *testing.T) {
	t.Parallel()

	t.Run("non-tenant", func(t *testing.T) {
		p := &ResourcePlan{
			ProtoFile:  "fixture.proto",
			TableName:  "things",
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
				{DBName: "email", Kind: KindScalar, JetGoType: "string", JetFieldName: "Email", Unique: true},
			},
		}
		got := renderSchemaSQL(p)
		assert.Contains(t, got, "CREATE UNIQUE INDEX idx_things_email_unique ON things (email);")
		assert.NotContains(t, got, "tenant_id")
	})

	t.Run("tenant-scoped", func(t *testing.T) {
		p := &ResourcePlan{
			ProtoFile:  "fixture.proto",
			TableName:  "things",
			PrimaryKey: []string{"tenant_id", "id"},
			Tenancy:    &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
			Columns: []ColumnPlan{
				{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
				{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
				{DBName: "email", Kind: KindScalar, JetGoType: "string", JetFieldName: "Email", Unique: true},
			},
		}
		got := renderSchemaSQL(p)
		assert.Contains(t, got, "CREATE UNIQUE INDEX idx_things_email_unique ON things (tenant_id, email);")
	})

	t.Run("unique-on-tenant-column", func(t *testing.T) {
		// If the unique column IS the tenant column, don't
		// double-list it — emit `(tenant_id)` once. Edge case
		// almost nobody will hit, but the logic shouldn't
		// produce garbage.
		p := &ResourcePlan{
			ProtoFile:  "fixture.proto",
			TableName:  "things",
			PrimaryKey: []string{"tenant_id"},
			Tenancy:    &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
			Columns: []ColumnPlan{
				{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID", Unique: true},
			},
		}
		got := renderSchemaSQL(p)
		assert.Contains(t, got, "CREATE UNIQUE INDEX idx_things_tenant_id_unique ON things (tenant_id);")
		// Must not accidentally emit `(tenant_id, tenant_id)`.
		assert.NotContains(t, got, "(tenant_id, tenant_id)")
	})

	t.Run("user-scoped", func(t *testing.T) {
		// User-scoped tables must also include user_id in the auto-
		// scope. Otherwise a `unique: true` on a per-user column
		// (e.g. "oauth connection name") would reject user B's value
		// when user A already has it within the same tenant — leaking
		// cross-user state via the constraint-violation error.
		p := &ResourcePlan{
			ProtoFile:  "fixture.proto",
			TableName:  "oauth_connections",
			PrimaryKey: []string{"tenant_id", "user_id", "name"},
			Tenancy: &TenancyPlan{
				Column: "tenant_id", UserScoped: true, UserColumn: "user_id", RuntimeRole: "app-tenant",
			},
			Columns: []ColumnPlan{
				{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
				{DBName: "user_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "UserID"},
				{DBName: "name", Kind: KindScalar, JetFieldName: "Name", Unique: true},
			},
		}
		got := renderSchemaSQL(p)
		assert.Contains(t, got, "CREATE UNIQUE INDEX idx_oauth_connections_name_unique ON oauth_connections (tenant_id, user_id, name);")
	})

	t.Run("unique-on-user-column", func(t *testing.T) {
		// Edge case: `unique: true` on the user column itself. Don't
		// double-list — emit `(tenant_id, user_id)` once.
		p := &ResourcePlan{
			ProtoFile:  "fixture.proto",
			TableName:  "things",
			PrimaryKey: []string{"tenant_id", "user_id"},
			Tenancy: &TenancyPlan{
				Column: "tenant_id", UserScoped: true, UserColumn: "user_id", RuntimeRole: "app-tenant",
			},
			Columns: []ColumnPlan{
				{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
				{DBName: "user_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "UserID", Unique: true},
			},
		}
		got := renderSchemaSQL(p)
		assert.Contains(t, got, "CREATE UNIQUE INDEX idx_things_user_id_unique ON things (tenant_id, user_id);")
		assert.NotContains(t, got, "(tenant_id, user_id, user_id)")
	})
}

// TestRenderMigration_CustomSQL pins the escape-hatch block —
// verbatim statements with one terminating semicolon each, wrapped
// in a visible marker so migration authors notice they're looking
// at caller-supplied SQL rather than plugin-declared shape.
func TestRenderMigration_CustomSQL(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
		},
		CustomSQL: []string{
			"CREATE INDEX IF NOT EXISTS idx_things_brin_created ON things USING BRIN (created_at)",
			"GRANT SELECT ON things TO readonly_user",
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "-- custom_sql — caller-supplied escape hatch")
	assert.Contains(t, got, "CREATE INDEX IF NOT EXISTS idx_things_brin_created ON things USING BRIN (created_at);")
	assert.Contains(t, got, "GRANT SELECT ON things TO readonly_user;")
}

// TestRenderMigration_CustomSQLRenormalisesInput pins the
// defensive re-normalisation inside renderCustomSQL.
// resolveResource already strips trailing semicolons + empty
// entries, but a plan built directly (tests, future tools) could
// hand over non-normalised input. The renderer must never emit
// `stmt;;` or blank noise.
func TestRenderMigration_CustomSQLRenormalisesInput(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
		},
		CustomSQL: []string{
			// Trailing semicolon (caller forgot they don't need one).
			"GRANT SELECT ON things TO readonly_user;",
			// Trailing whitespace + semicolon.
			"CREATE INDEX foo ON things (id)  ; ",
			// Whitespace-only entry — must be dropped, not emitted as blank.
			"   ",
			"",
			// Multiple trailing semicolons — all stripped.
			"VACUUM things;;;",
		},
	}
	got := renderSchemaSQL(p)
	// Every emitted statement has exactly one terminating `;`.
	assert.Contains(t, got, "GRANT SELECT ON things TO readonly_user;\n")
	assert.Contains(t, got, "CREATE INDEX foo ON things (id);\n")
	assert.Contains(t, got, "VACUUM things;\n")
	// No doubled-up semicolons.
	assert.NotContains(t, got, ";;")
	// No blank lines inside the custom_sql block — the whitespace-
	// only entries got dropped.
	assert.NotRegexp(t, `--\s+custom_sql\s*\n\n`, got)
}

// TestRenderMigration_CustomSQLAllBlankNoMarker pins that a CustomSQL
// slice consisting entirely of whitespace-only entries produces no
// output at all — not even the marker comment. Previously the
// marker was emitted before the normalisation loop ran, so a
// directly-constructed plan with junk entries would produce a
// dangling `-- custom_sql` header with no statements below it.
func TestRenderMigration_CustomSQLAllBlankNoMarker(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
		},
		CustomSQL: []string{"   ", "", ";;;", " ; ", "\t\n"},
	}
	got := renderSchemaSQL(p)
	assert.NotContains(t, got, "custom_sql",
		"all-blank CustomSQL must not produce a dangling marker comment")
}

// TestRenderMigration_CustomSQLEmpty pins that an absent custom_sql
// list produces no marker comment — the escape-hatch block should
// be invisible when nobody used it.
func TestRenderMigration_CustomSQLEmpty(t *testing.T) {
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
		},
	}
	got := renderSchemaSQL(p)
	assert.NotContains(t, got, "custom_sql")
}

// TestJsonbPathExpr pins the JSON accessor chain shape the plugin
// emits for every jsonb_indexed_paths entry and every auto-
// registered FilterFields key. Both Column- and OneofColumn-level
// annotations route through this function, so a regression here
// would simultaneously break CREATE INDEX emission and AIP-160
// dotted-path filter translation.
//
// Rules:
//   - Single segment: `col->>'key'` — one `->>` terminator.
//   - Multi segment: intermediate hops use `->` (object traversal),
//     final hop uses `->>` (text coercion, btree-indexable).
func TestJsonbPathExpr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		col  string
		path string
		want string
	}{
		{name: "single segment", col: "config", path: "api_key", want: "config->>'api_key'"},
		{name: "two segment", col: "config", path: "nested.timeout", want: "config->'nested'->>'timeout'"},
		{name: "three segment", col: "cloud", path: "a.b.c", want: "cloud->'a'->'b'->>'c'"},
		{name: "deep camelCase", col: "spec", path: "storage.dataDiskGib", want: "spec->'storage'->>'dataDiskGib'"},
		// Unicode in the path segment — the validator allows these
		// (only empty and quote chars are rejected), so the emitter
		// must thread them through unescaped.
		{name: "unicode segment", col: "attrs", path: "日本語", want: "attrs->>'日本語'"},
		// Hyphens in segments (legal in JSON keys) round-trip
		// unchanged.
		{name: "hyphenated segment", col: "attrs", path: "with-dash.deep", want: "attrs->'with-dash'->>'deep'"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, jsonbPathExpr(tt.col, tt.path))
		})
	}
}

// TestJsonbPathIndexName pins the synthesized index-name shape.
// The name has to be lowercase (PG folds unquoted identifiers) and
// idempotent across path punctuation (dots → underscores) so
// pg_indexes lookups and pg_schema_diff round-trips see a stable
// identifier — if the synthesiser drifted from what emit_sql.go
// emits in a CREATE INDEX, drift-check would report phantom changes.
func TestJsonbPathIndexName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		table string
		col   string
		path  string
		want  string
	}{
		{name: "single segment", table: "things", col: "config", path: "api_key", want: "idx_things_config_api_key"},
		{name: "dotted", table: "things", col: "config", path: "a.b.c", want: "idx_things_config_a_b_c"},
		// PG folds the column name to lowercase too — if an author
		// wrote UpperCase somehow, the synth must match what PG
		// would store.
		{name: "camelCase path lowercased", table: "things", col: "spec", path: "installPackVersion", want: "idx_things_spec_installpackversion"},
		{name: "camelCase nested", table: "cluster_like", col: "spec", path: "storage.dataDiskGib", want: "idx_cluster_like_spec_storage_datadiskgib"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, jsonbPathIndexName(tt.table, tt.col, tt.path))
		})
	}
}

// TestRenderMigration_JSONBPathIndex_Oneof pins the oneof-column
// branch of renderJSONBPathIndexes. Integration coverage exists
// via the ClusterLike fixture, but a unit-level case documents
// the expected emission shape without needing a testcontainer
// and makes the OneofColumnPlan code path independently testable.
func TestRenderMigration_JSONBPathIndex_Oneof(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:          "cloud",
				KindColumn:        "cloud_kind",
				JSONColumn:        "cloud",
				JetKindField:      "CloudKind",
				JetJSONField:      "Cloud",
				Variants:          []OneofVariant{{VariantName: "aws"}, {VariantName: "gcp"}},
				JSONBIndexedPaths: []string{"accountId", "nested.projectId"},
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE INDEX idx_things_cloud_accountid ON things ((cloud->>'accountId'));")
	assert.Contains(t, got, "CREATE INDEX idx_things_cloud_nested_projectid ON things ((cloud->'nested'->>'projectId'));")
}

// TestRenderMigration_JSONBPathIndex_MixedColumnAndOneof pins that
// a table with BOTH column-level and oneof-level jsonb_indexed_paths
// emits both sets of indexes, not just one. Earlier draft of the
// renderJSONBPathIndexes "any" check short-circuited after finding
// Column entries and never iterated OneofColumns; regression test
// against that specific mistake.
func TestRenderMigration_JSONBPathIndex_MixedColumnAndOneof(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
			{
				DBName: "spec", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBProto, JetFieldName: "Spec",
				JSONBIndexedPaths: []string{"version"},
			},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:          "cloud",
				KindColumn:        "cloud_kind",
				JSONColumn:        "cloud",
				JetKindField:      "CloudKind",
				JetJSONField:      "Cloud",
				Variants:          []OneofVariant{{VariantName: "aws"}},
				JSONBIndexedPaths: []string{"accountId"},
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE INDEX idx_things_spec_version ON things ((spec->>'version'));")
	assert.Contains(t, got, "CREATE INDEX idx_things_cloud_accountid ON things ((cloud->>'accountId'));")
}

// TestRenderMigration_EmissionOrder pins the section ordering in
// the DDL. The sections that exist (create table, rename
// directives, indexes, unique-column indexes, JSONB path indexes,
// GIN indexes, RLS policies, comments, custom_sql) must always
// appear in that order — a reviewer scanning the emitted migration
// relies on the order for context, and pg-schema-diff's idempotency
// assumptions depend on custom_sql landing last so earlier sections
// don't reference entities created by the escape-hatch block.
func TestRenderMigration_EmissionOrder(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:    "fixture.proto",
		TableName:    "things",
		TableComment: "the things table",
		PrimaryKey:   []string{"tenant_id", "id"},
		Tenancy:      &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID", Comment: "the id"},
			{
				DBName: "email", SQLType: "TEXT", NotNull: true, DefaultExpr: "''",
				Kind: KindScalar, JetGoType: "string", JetFieldName: "Email",
				Unique: true, RenameFrom: "email_address",
			},
			{
				DBName: "spec", SQLType: "JSONB", NotNull: true, DefaultExpr: "'{}'::jsonb",
				Kind: KindJSONBProto, JetFieldName: "Spec",
				JSONBIndexedPaths: []string{"version"},
				JSONBGinIndex:     true,
			},
		},
		Indexes: []IndexPlan{
			{Name: "idx_things_id", Columns: []string{"id"}, Order: []string{"ASC"}},
		},
		CustomSQL: []string{"CREATE INDEX IF NOT EXISTS idx_things_created_at_brin ON things USING BRIN (created_at)"},
	}
	got := renderSchemaSQL(p)

	// Map each section to its starting character offset. A section
	// MUST appear in the output and MUST appear before later sections.
	find := func(marker string) int {
		i := strings.Index(got, marker)
		require.GreaterOrEqual(t, i, 0, "marker %q not found in:\n%s", marker, got)
		return i
	}
	creatTable := find("CREATE TABLE things")
	renameDirective := find("PLUGIN-RENAME: things email_address -> email")
	explicitIndex := find("CREATE INDEX idx_things_id ON things")
	uniqueIndex := find("CREATE UNIQUE INDEX idx_things_email_unique")
	jsonbPathIndex := find("CREATE INDEX idx_things_spec_version")
	ginIndex := find("CREATE INDEX idx_things_spec_gin")
	rls := find("ENABLE ROW LEVEL SECURITY")
	tableComment := find("COMMENT ON TABLE things IS $$the things table$$")
	customSQL := find("-- custom_sql")

	// The order a migration reviewer expects to see:
	//   CREATE TABLE → rename directives → explicit indexes →
	//   unique-column indexes → JSONB path indexes → GIN indexes →
	//   RLS → comments → custom_sql
	assert.Less(t, creatTable, renameDirective, "rename directives after CREATE TABLE")
	assert.Less(t, renameDirective, explicitIndex, "explicit indexes after rename directives")
	assert.Less(t, explicitIndex, uniqueIndex, "unique indexes after explicit indexes")
	assert.Less(t, uniqueIndex, jsonbPathIndex, "JSONB path indexes after unique indexes")
	assert.Less(t, jsonbPathIndex, ginIndex, "GIN indexes after JSONB path indexes")
	assert.Less(t, ginIndex, rls, "RLS after GIN indexes")
	assert.Less(t, rls, tableComment, "comments after RLS")
	assert.Less(t, tableComment, customSQL, "custom_sql appears last")
}

// TestPgIdent pins the Postgres identifier quoting rule —
// double-quoted identifier with any embedded `"` escaped to `""`.
// Used for role names inside RLS CREATE POLICY statements. Go's
// `fmt %q` is NOT equivalent: it uses backslash-quote escaping,
// which PG doesn't parse as quote-escape. If we ever drifted back
// to %q, role names containing `"` would produce malformed DDL.
func TestPgIdent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"app-tenant", `"app-tenant"`},
		{"plain", `"plain"`},
		{"", `""`},
		{`has"quote`, `"has""quote"`},
		{`"both"ends"`, `"""both""ends"""`},
		// Pathological: an embedded double-quote followed by another
		// must produce two pairs of doubled quotes in the output.
		{`a""b`, `"a""""b"`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pgIdent(tt.in))
		})
	}
}

// TestRenderMigration_OneofComment pins that a leading comment on
// the proto oneof declaration flows to BOTH the kind discriminator
// column and the JSON value column via COMMENT ON COLUMN. Duplicates
// one string across two catalog entries, but the documentation
// value — `\d+` on either column shows the oneof's purpose —
// outweighs the space cost.
func TestRenderMigration_OneofComment(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "cloud",
				KindColumn:   "cloud_kind",
				JSONColumn:   "cloud",
				JetKindField: "CloudKind",
				JetJSONField: "Cloud",
				Variants:     []OneofVariant{{VariantName: "aws"}, {VariantName: "gcp"}},
				Comment:      "active cloud-provider variant — exactly one of aws/gcp is set",
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "COMMENT ON COLUMN things.cloud_kind IS $$active cloud-provider variant — exactly one of aws/gcp is set$$;")
	assert.Contains(t, got, "COMMENT ON COLUMN things.cloud IS $$active cloud-provider variant — exactly one of aws/gcp is set$$;")
}

// TestRenderMigration_OneofCommentSectionGate pins that a plan with
// NO comments at all — table, column, or oneof — produces no
// trailing COMMENT ON section at all. Without the gate an empty
// comments block would still emit its leading newline.
func TestRenderMigration_OneofCommentSectionGate(t *testing.T) {
	t.Parallel()
	// Plan with a oneof but no comments anywhere.
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "things",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName:     "cloud",
				KindColumn:   "cloud_kind",
				JSONColumn:   "cloud",
				JetKindField: "CloudKind",
				JetJSONField: "Cloud",
				Variants:     []OneofVariant{{VariantName: "aws"}},
				// Comment intentionally empty.
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.NotContains(t, got, "COMMENT ON")
}

// TestRenderMigration_OneofCommentAlongsideRegularComments pins the
// emission ordering — regular column comments come first (in
// orderedColumns sequence), then oneof comments (kind first, then
// JSON). Pairs with the emission-order test that pins section-
// level order; this one pins intra-section order.
func TestRenderMigration_OneofCommentAlongsideRegularComments(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:    "fixture.proto",
		TableName:    "things",
		PrimaryKey:   []string{"id"},
		TableComment: "the things table",
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID", Comment: "the id"},
		},
		OneofColumns: []OneofColumnPlan{
			{
				BaseName: "cloud", KindColumn: "cloud_kind", JSONColumn: "cloud",
				Variants: []OneofVariant{{VariantName: "aws"}},
				Comment:  "cloud provider",
			},
		},
	}
	got := renderSchemaSQL(p)
	iTable := strings.Index(got, "COMMENT ON TABLE")
	iRegular := strings.Index(got, "COMMENT ON COLUMN things.id")
	iOneofKind := strings.Index(got, "COMMENT ON COLUMN things.cloud_kind")
	iOneofJSON := strings.Index(got, "COMMENT ON COLUMN things.cloud IS")
	require.Greater(t, iTable, 0)
	require.Greater(t, iRegular, 0)
	require.Greater(t, iOneofKind, 0)
	require.Greater(t, iOneofJSON, 0)
	assert.Less(t, iTable, iRegular, "table comment precedes column comments")
	assert.Less(t, iRegular, iOneofKind, "regular column comments precede oneof kind")
	assert.Less(t, iOneofKind, iOneofJSON, "oneof kind column comment precedes oneof json value comment")
}

// TestRenderMigration_GeneratedColumn pins the DDL shape of a stored
// generated column: `<type> GENERATED ALWAYS AS (<expr>) STORED NOT
// NULL`, no DEFAULT clause, expression body emitted verbatim. A
// regular column on the same table must still render with the
// pre-existing `<type> NOT NULL DEFAULT <expr>` shape, so the new
// branch can't regress non-generated emission.
func TestRenderMigration_GeneratedColumn(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "rollups",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID", JetGoType: "string"},
			{DBName: "input_microcents", SQLType: "BIGINT", NotNull: true, DefaultExpr: "0", Kind: KindScalar, JetFieldName: "InputMicrocents", JetGoType: "int64"},
			{DBName: "output_microcents", SQLType: "BIGINT", NotNull: true, DefaultExpr: "0", Kind: KindScalar, JetFieldName: "OutputMicrocents", JetGoType: "int64"},
			{
				DBName:        "total_microcents",
				SQLType:       "BIGINT",
				NotNull:       true,
				Kind:          KindScalar,
				JetFieldName:  "TotalMicrocents",
				JetGoType:     "int64",
				GeneratedExpr: "input_microcents + output_microcents",
			},
		},
	}

	got := renderSchemaSQL(p)

	// Generated column: GENERATED ALWAYS AS (<expr>) STORED, NOT NULL
	// after STORED, no DEFAULT.
	assert.Regexp(t, `total_microcents\s+BIGINT GENERATED ALWAYS AS \(input_microcents \+ output_microcents\) STORED NOT NULL`, got,
		"generated column must render with GENERATED ALWAYS AS (...) STORED NOT NULL")
	assert.NotContains(t, got, "total_microcents  BIGINT GENERATED ALWAYS AS (input_microcents + output_microcents) STORED NOT NULL DEFAULT",
		"generated column must not carry a DEFAULT clause")

	// Regular columns retain the pre-existing shape — the new branch
	// must not change non-generated emission.
	assert.Regexp(t, `input_microcents\s+BIGINT NOT NULL DEFAULT 0`, got)
	assert.Regexp(t, `output_microcents\s+BIGINT NOT NULL DEFAULT 0`, got)
}

// TestRenderMigration_GeneratedColumnNullable pins the rare case
// where a generated column is declared nullable: GENERATED ALWAYS AS
// (...) STORED with no NOT NULL, no DEFAULT.
func TestRenderMigration_GeneratedColumnNullable(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "rollups",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, DefaultExpr: "''", Kind: KindScalar, JetFieldName: "ID", JetGoType: "string"},
			{
				DBName:        "computed",
				SQLType:       "BIGINT",
				NotNull:       false,
				Nullable:      true,
				Kind:          KindScalar,
				JetFieldName:  "Computed",
				JetGoType:     "*int64",
				GeneratedExpr: "1 + 1",
			},
		},
	}

	got := renderSchemaSQL(p)
	assert.Regexp(t, `computed\s+BIGINT GENERATED ALWAYS AS \(1 \+ 1\) STORED,`, got,
		"nullable generated column omits NOT NULL after STORED")
	assert.NotContains(t, got, "computed  BIGINT GENERATED ALWAYS AS (1 + 1) STORED NOT NULL")
}
