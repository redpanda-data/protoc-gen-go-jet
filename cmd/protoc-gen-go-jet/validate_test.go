package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planFixture builds a minimal valid plan — two persisted scalar
// columns plus a tenant synthesized column and a PK on (tenant_id,
// name). Tests mutate the returned plan to probe one rule at a time.
func planFixture() *ResourcePlan {
	return &ResourcePlan{
		TableName: "things",
		Columns: []ColumnPlan{
			{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
			{DBName: "name", Kind: KindScalar, JetFieldName: "Name", Orderable: true, JetGoType: "string"},
			{DBName: "display_name", Kind: KindScalar, JetFieldName: "DisplayName"},
		},
		PrimaryKey: []string{"tenant_id", "name"},
	}
}

func TestValidatePlan_HappyPath(t *testing.T) {
	t.Parallel()
	require.NoError(t, validatePlan(planFixture()))
}

func TestValidatePlan_RequiresPrimaryKey(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.PrimaryKey = nil

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary_key is required")
}

func TestValidatePlan_PrimaryKeyReferencesUnknownColumn(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.PrimaryKey = []string{"tenant_id", "ghost"}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary_key")
	assert.Contains(t, err.Error(), `"ghost"`)
}

func TestValidatePlan_DuplicateColumnName(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Columns = append(p.Columns, ColumnPlan{DBName: "name", Kind: KindScalar, JetFieldName: "NameDup"})

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "column name")
	assert.Contains(t, err.Error(), "twice")
}

// TestValidatePlan_ProtoFieldCollidesWithSynthesizedTenantColumn pins
// the specialised error message for the common case of a proto field
// overriding its name to land on the tenancy column. The generic
// "two proto fields collapse" hint would misdirect the author toward
// renaming a second proto field that doesn't exist; the specialised
// message names the real fix (rename the proto column or change
// tenancy.column).
func TestValidatePlan_ProtoFieldCollidesWithSynthesizedTenantColumn(t *testing.T) {
	t.Parallel()
	// planFixture has tenant_id as a synthesised column already.
	// Append a non-synthesised proto field trying to use the same
	// DB name — common footgun when a user uses column.name to pin
	// a field onto tenant_id.
	p := planFixture()
	p.Columns = append(p.Columns, ColumnPlan{DBName: "tenant_id", Kind: KindScalar, JetFieldName: "TenantIDProto"})

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin-synthesised tenancy column")
	assert.Contains(t, err.Error(), "tenancy.column")
}

// TestValidatePlan_TenancyRequiresTenantColumnInPK pins the
// tenant-scoped-PK footgun caught while walking Journey A: a
// tenant-annotated table whose PK omits the tenant column enforces
// global uniqueness across tenants, so a PK clash leaks cross-tenant
// state via the duplicate-key error.
func TestValidatePlan_TenancyRequiresTenantColumnInPK(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Tenancy = &TenancyPlan{Column: "tenant_id", RuntimeRole: "e2e-tenant"}
	p.PrimaryKey = []string{"name"} // tenant_id dropped

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenancy.column")
	assert.Contains(t, err.Error(), "global")
}

// TestValidatePlan_TenancyPKPresenceHappyPath confirms the guard
// doesn't regress the standard (tenant_id, name) PK a typical
// resources use.
func TestValidatePlan_TenancyPKPresenceHappyPath(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Tenancy = &TenancyPlan{Column: "tenant_id", RuntimeRole: "e2e-tenant"}
	// PK already (tenant_id, name) per fixture.

	require.NoError(t, validatePlan(p))
}

// TestValidatePlan_TenancyRequiresTenantColumnInUniqueIndex pins
// the sibling footgun to the tenant-PK guard: a UNIQUE index
// declared via Table.indexes that omits the tenant column enforces
// global uniqueness across tenants, exposing another tenant's row
// via the collision error.
func TestValidatePlan_TenancyRequiresTenantColumnInUniqueIndex(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Tenancy = &TenancyPlan{Column: "tenant_id", RuntimeRole: "e2e-tenant"}
	p.Indexes = []IndexPlan{{
		Name:    "idx_things_name_unique",
		Columns: []string{"name"}, // tenant_id missing
		Order:   []string{"ASC"},
		Unique:  true,
	}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unique index")
	assert.Contains(t, err.Error(), "global")
}

// TestValidatePlan_TenancyAllowsPartialUniqueIndexWithoutTenant pins
// that a partial unique index (where: ... set) is exempt from the
// tenant-presence check. Callers reaching for `where:` have opted
// into a specific uniqueness window; a global-scope window is a
// valid shape we shouldn't over-constrain.
func TestValidatePlan_TenancyAllowsPartialUniqueIndexWithoutTenant(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Tenancy = &TenancyPlan{Column: "tenant_id", RuntimeRole: "e2e-tenant"}
	p.Indexes = []IndexPlan{{
		Name:    "idx_things_name_active_unique",
		Columns: []string{"name"},
		Order:   []string{"ASC"},
		Unique:  true,
		Where:   "status = 'active'",
	}}

	require.NoError(t, validatePlan(p))
}

// TestValidatePlan_TenancyNonUniqueIndexDoesNotTrip confirms the
// guard only fires on UNIQUE indexes. A plain secondary index
// without tenant_id is fine (perf may suffer on tenant-filtered
// queries, but it's not a correctness issue).
func TestValidatePlan_TenancyNonUniqueIndexDoesNotTrip(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Tenancy = &TenancyPlan{Column: "tenant_id", RuntimeRole: "e2e-tenant"}
	p.Indexes = []IndexPlan{{
		Name:    "idx_things_display_name",
		Columns: []string{"display_name"},
		Order:   []string{"ASC"},
		Unique:  false,
	}}

	require.NoError(t, validatePlan(p))
}

// userScopedFixture builds on planFixture with a synthesised
// `user_id` column and a (tenant_id, user_id, name) PK — the e2e
// UserScoped shape. Tests mutate the PK / indexes to probe the
// user-scoped variant of the cross-tenant-uniqueness guards.
func userScopedFixture() *ResourcePlan {
	p := planFixture()
	p.Tenancy = &TenancyPlan{
		Column:      "tenant_id",
		RuntimeRole: "e2e-tenant",
		UserScoped:  true,
		UserColumn:  "user_id",
	}
	p.Columns = append(p.Columns, ColumnPlan{
		DBName: "user_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "UserID",
	})
	p.PrimaryKey = []string{"tenant_id", "user_id", "name"}
	return p
}

// TestValidatePlan_UserScopedRequiresUserColumnInPK pins the
// user-scoped variant of the PK footgun: omitting `user_id` from
// the PK lets two users within the same tenant collide on the
// remaining columns, and the loser's duplicate-key error leaks
// another user's existence.
func TestValidatePlan_UserScopedRequiresUserColumnInPK(t *testing.T) {
	t.Parallel()
	p := userScopedFixture()
	p.PrimaryKey = []string{"tenant_id", "name"} // user_id dropped

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenancy.user_column")
	assert.Contains(t, err.Error(), "users within the same tenant")
}

// TestValidatePlan_UserScopedRequiresUserColumnInUniqueIndex pins
// the user-scoped variant for UNIQUE indexes declared via
// Table.indexes.
func TestValidatePlan_UserScopedRequiresUserColumnInUniqueIndex(t *testing.T) {
	t.Parallel()
	p := userScopedFixture()
	p.Indexes = []IndexPlan{{
		Name:    "idx_things_tenant_name_unique",
		Columns: []string{"tenant_id", "name"}, // user_id missing
		Order:   []string{"ASC", "ASC"},
		Unique:  true,
	}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenancy.user_column")
	assert.Contains(t, err.Error(), "users within the same tenant")
}

// TestValidatePlan_UserScopedHappyPath pins that the standard
// (tenant_id, user_id, name) shape survives both guards.
func TestValidatePlan_UserScopedHappyPath(t *testing.T) {
	t.Parallel()
	p := userScopedFixture()
	// PK already (tenant_id, user_id, name) per fixture.
	p.Indexes = []IndexPlan{{
		Name:    "idx_things_all_unique",
		Columns: []string{"tenant_id", "user_id", "name"},
		Order:   []string{"ASC", "ASC", "ASC"},
		Unique:  true,
	}}

	require.NoError(t, validatePlan(p))
}

// TestValidatePlan_OneofKindCollidesWithScalar — the oneof pair
// columns (<base>_kind, <base>) mustn't shadow an existing scalar.
// Without the guard, CREATE TABLE would emit two columns with the
// same identifier and the first apply would fail with a PG syntax
// error that doesn't name the proto.
func TestValidatePlan_OneofKindCollidesWithScalar(t *testing.T) {
	t.Parallel()
	p := planFixture()
	// Add a scalar "config_kind", then a oneof whose kind column collides.
	p.Columns = append(p.Columns, ColumnPlan{DBName: "config_kind", Kind: KindScalar, JetFieldName: "ConfigKind"})
	p.OneofColumns = []OneofColumnPlan{{
		BaseName:   "config",
		KindColumn: "config_kind",
		JSONColumn: "config",
	}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "oneof")
	assert.Contains(t, err.Error(), "config_kind")
}

func TestValidatePlan_OneofJSONCollidesWithScalar(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Columns = append(p.Columns, ColumnPlan{DBName: "config", Kind: KindScalar, JetFieldName: "Config"})
	p.OneofColumns = []OneofColumnPlan{{
		BaseName:   "config",
		KindColumn: "config_kind",
		JSONColumn: "config",
	}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON column")
	assert.Contains(t, err.Error(), "config")
}

func TestValidatePlan_DuplicateIndexName(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Indexes = []IndexPlan{
		{Name: "idx_things_name", Columns: []string{"name"}},
		{Name: "idx_things_name", Columns: []string{"name"}},
	}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "index name")
	assert.Contains(t, err.Error(), "twice")
}

func TestValidatePlan_IndexReferencesUnknownColumn(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.Indexes = []IndexPlan{{Name: "idx_ghost", Columns: []string{"ghost"}}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idx_ghost")
	assert.Contains(t, err.Error(), "ghost")
}

func TestValidatePlan_DefaultOrderUnknownField(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.DefaultOrder = []OrderPart{{FieldPath: "ghost"}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_order_by")
	assert.Contains(t, err.Error(), "ghost")
}

// TestValidatePlan_DefaultOrderNotOrderable catches the common mistake
// where an author adds a column to default_order_by but forgets to
// mark it orderable. Without the guard, the generated aipjet.Schema
// would panic at NewSchema time with a cryptic message.
func TestValidatePlan_DefaultOrderNotOrderable(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.DefaultOrder = []OrderPart{{FieldPath: "display_name"}} // display_name is NOT orderable

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "display_name")
	assert.Contains(t, err.Error(), "orderable")
}

func TestValidatePlan_TieBreakerUnknownField(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.TieBreaker = []OrderPart{{FieldPath: "ghost"}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tie_breaker")
	assert.Contains(t, err.Error(), "ghost")
}

func TestValidatePlan_TieBreakerNotOrderable(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.TieBreaker = []OrderPart{{FieldPath: "display_name"}}

	err := validatePlan(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tie_breaker")
	assert.Contains(t, err.Error(), "display_name")
	assert.Contains(t, err.Error(), "orderable")
}

// TestValidatePlan_SynthesizedTenantIDIsImplicitlyOrderable pins the
// small convenience: synthesized columns (tenant_id / user_id) are
// always usable in tie-breakers even without an explicit Orderable
// flag — they'd have nowhere to set it anyway since they're not
// proto fields.
func TestValidatePlan_SynthesizedTenantIDIsImplicitlyOrderable(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.TieBreaker = []OrderPart{{FieldPath: "tenant_id"}}

	assert.NoError(t, validatePlan(p))
}

// TestValidatePlan_CrossOneofVariantNameCollisionAllowed documents
// that validatePlan deliberately does NOT flag two separate oneofs
// sharing a VariantName. Each oneof has its own kind column, so the
// discriminator is scoped per-column — 'static' under `auth_kind` and
// 'static' under `credentials_kind` never appear in the same CHECK
// constraint and never collide at the DB level.
//
// A human reader scanning the proto might still find two oneofs with
// overlapping variant names confusing. That's a style issue, not a
// correctness issue, and belongs in a lint pass or code review — not
// here. This test stands as the pin: don't add a guard unless you
// have a concrete bug it prevents.
func TestValidatePlan_CrossOneofVariantNameCollisionAllowed(t *testing.T) {
	t.Parallel()
	p := planFixture()
	p.OneofColumns = []OneofColumnPlan{
		{
			BaseName:   "auth",
			KindColumn: "auth_kind",
			JSONColumn: "auth",
			Variants:   []OneofVariant{{VariantName: "static"}, {VariantName: "oauth"}},
		},
		{
			BaseName:   "credentials",
			KindColumn: "credentials_kind",
			JSONColumn: "credentials",
			// 'static' appears in both oneofs — validatePlan must accept.
			Variants: []OneofVariant{{VariantName: "static"}, {VariantName: "dynamic"}},
		},
	}
	require.NoError(t, validatePlan(p), "cross-oneof variant name overlap is not a correctness issue")
}

// TestValidatePlan_JSONBIndexedPathDuplicate pins that declaring the
// same path twice on one column is rejected — both emissions would
// produce identical CREATE INDEX statements and PG would refuse the
// second at apply time.
func TestValidatePlan_JSONBIndexedPathDuplicate(t *testing.T) {
	t.Parallel()
	p := planFixture()
	// Swap the config column into a JSONB-backed kind so the
	// jsonb_indexed_paths validation applies.
	for i := range p.Columns {
		if p.Columns[i].DBName == "display_name" {
			p.Columns[i].Kind = KindJSONBProto
			p.Columns[i].JSONBIndexedPaths = []string{"api_key", "api_key"}
		}
	}
	err := validatePlan(p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "twice")
	}
}

// TestValidatePlan_JSONBIndexedPathIndexNameCollision pins that an
// explicit index and a synthesized jsonb_indexed_paths index can't
// end up with the same name — the synthesized name uses
// `idx_<table>_<col>_<path>`, so an explicit index with that literal
// name would collide.
func TestValidatePlan_JSONBIndexedPathIndexNameCollision(t *testing.T) {
	t.Parallel()
	p := planFixture()
	for i := range p.Columns {
		if p.Columns[i].DBName == "display_name" {
			p.Columns[i].Kind = KindJSONBProto
			p.Columns[i].JSONBIndexedPaths = []string{"api_key"}
		}
	}
	p.Indexes = append(p.Indexes, IndexPlan{
		Name:    "idx_things_display_name_api_key",
		Columns: []string{"name"},
		Order:   []string{"ASC"},
	})
	err := validatePlan(p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "collides with an existing index")
	}
}

// TestValidatePlan_UniqueIndexNameCollision pins that an explicit
// index whose name matches the synthesised unique-column index name
// is rejected at plan-build time. Without this, CREATE UNIQUE INDEX
// would duplicate the explicit one at apply time.
func TestValidatePlan_UniqueIndexNameCollision(t *testing.T) {
	t.Parallel()
	p := planFixture()
	for i := range p.Columns {
		if p.Columns[i].DBName == "display_name" {
			p.Columns[i].JetGoType = "string"
			p.Columns[i].Unique = true
		}
	}
	p.Indexes = append(p.Indexes, IndexPlan{
		Name:    "idx_things_display_name_unique",
		Columns: []string{"name"},
		Order:   []string{"ASC"},
	})
	err := validatePlan(p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "collides with an existing index")
	}
}

// TestValidatePlan_GinIndexNameCollision — same for the `_gin`
// suffix used by jsonb_gin_index.
func TestValidatePlan_GinIndexNameCollision(t *testing.T) {
	t.Parallel()
	p := planFixture()
	for i := range p.Columns {
		if p.Columns[i].DBName == "display_name" {
			p.Columns[i].Kind = KindJSONBProto
			p.Columns[i].JSONBGinIndex = true
		}
	}
	p.Indexes = append(p.Indexes, IndexPlan{
		Name:    "idx_things_display_name_gin",
		Columns: []string{"name"},
		Order:   []string{"ASC"},
	})
	err := validatePlan(p)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "collides with an existing index")
	}
}

// TestValidatePlan_SynthIdentTooLong pins the 63-byte Postgres
// identifier cap on PLUGIN-SYNTHESISED names. User-supplied names
// are already capped by validateSQLIdent; this guard catches the
// compound case where each input is short enough individually but
// their concatenation overshoots. Concrete shape: CHECK constraint
// name synthesises as `<table>_<col>_check` — a 40-byte table +
// a 25-byte column name each pass validateSQLIdent but produce a
// 72-byte constraint name.
func TestValidatePlan_SynthIdentTooLong(t *testing.T) {
	t.Parallel()

	t.Run("check constraint name overflow", func(t *testing.T) {
		t.Parallel()
		// Table name 55 bytes + `_id_check` (9 bytes) = 64 — one over.
		p := &ResourcePlan{
			TableName:  "a_very_long_table_name_indeed_for_testing_overflow_edge",
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				{DBName: "id", Kind: KindScalar, JetGoType: "string", JetFieldName: "ID", Check: "id <> ''"},
			},
		}
		err := validatePlan(p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "exceeds Postgres NAMEDATALEN")
			assert.Contains(t, err.Error(), "CHECK")
		}
	})

	t.Run("unique index name overflow", func(t *testing.T) {
		t.Parallel()
		p := &ResourcePlan{
			TableName:  "a_very_long_table_name_1234567890abcdef_more_padding",
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
				{DBName: "some_very_long_field_name_that_is_long", Kind: KindScalar, JetGoType: "string", JetFieldName: "F", Unique: true},
			},
		}
		err := validatePlan(p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "exceeds Postgres NAMEDATALEN")
			assert.Contains(t, err.Error(), "unique")
		}
	})

	t.Run("jsonb_indexed_paths name overflow", func(t *testing.T) {
		t.Parallel()
		// Table + column + path all passing validateSQLIdent but
		// their concat as `idx_<table>_<col>_<path>` overshoots.
		p := &ResourcePlan{
			TableName:  "long_table_name_12345678",
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				{DBName: "id", Kind: KindScalar, JetFieldName: "ID"},
				{
					DBName: "spec_column_field", SQLType: "JSONB", Kind: KindJSONBProto, JetFieldName: "Spec",
					JSONBIndexedPaths: []string{"a.very.long.nested.path.going.deep"},
				},
			},
		}
		err := validatePlan(p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "exceeds Postgres NAMEDATALEN")
		}
	})
}

// TestValidatePlan_UniqueOnSolePrimaryKeyRejected pins the redundancy
// guard — `unique: true` on a column that's ALSO the sole primary
// key would emit a UNIQUE INDEX duplicating what the PK constraint
// already provides. Composite primary keys don't trip this: a
// single-column `unique: true` inside a composite PK still adds
// value (enforces independent uniqueness on that column).
func TestValidatePlan_UniqueOnSolePrimaryKeyRejected(t *testing.T) {
	t.Parallel()

	t.Run("sole PK rejected", func(t *testing.T) {
		t.Parallel()
		p := &ResourcePlan{
			TableName:  "things",
			PrimaryKey: []string{"id"},
			Columns: []ColumnPlan{
				{DBName: "id", Kind: KindScalar, JetGoType: "string", JetFieldName: "ID", Unique: true, Orderable: true},
			},
		}
		err := validatePlan(p)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "redundant")
			assert.Contains(t, err.Error(), "sole primary key")
		}
	})

	t.Run("composite PK allows unique on a member", func(t *testing.T) {
		t.Parallel()
		p := &ResourcePlan{
			TableName:  "things",
			PrimaryKey: []string{"tenant_id", "id"},
			Columns: []ColumnPlan{
				{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID"},
				{DBName: "id", Kind: KindScalar, JetGoType: "string", JetFieldName: "ID", Unique: true, Orderable: true},
			},
		}
		assert.NoError(t, validatePlan(p), "unique on one member of a composite PK is not redundant")
	})
}

// TestValidateUniqueIndexable_Rejects pins which kinds / go types
// can't carry `unique: true`. Bool is a modelling mistake (at most
// two rows); JSONB / arrays need a special operator class.
func TestValidateUniqueIndexable_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    ColumnPlan
		want string
	}{
		{name: "bool", c: ColumnPlan{Kind: KindScalar, JetGoType: "bool"}, want: "bool columns admit at most"},
		{name: "nullable bool", c: ColumnPlan{Kind: KindScalar, JetGoType: "*bool"}, want: "bool columns admit at most"},
		{name: "jsonb", c: ColumnPlan{Kind: KindJSONBProto, JetGoType: "string"}, want: "scalar / timestamp"},
		{name: "repeated", c: ColumnPlan{Kind: KindRepeatedText, JetGoType: "pq.StringArray"}, want: "scalar / timestamp"},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateUniqueIndexable(&tt.c)
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tt.want)
			}
		})
	}
}

// TestValidateUniqueIndexable_Accepts pins that every kind with a
// sensible btree index is allowed. If a future kind becomes
// uniqueable (e.g. we add a money type), add a case here.
func TestValidateUniqueIndexable_Accepts(t *testing.T) {
	t.Parallel()
	for _, c := range []ColumnPlan{
		{Kind: KindScalar, JetGoType: "string"},
		{Kind: KindScalar, JetGoType: "int32"},
		{Kind: KindScalar, JetGoType: "int64"},
		{Kind: KindScalar, JetGoType: "float32"},
		{Kind: KindScalar, JetGoType: "float64"},
		{Kind: KindScalar, JetGoType: "[]byte"},
		{Kind: KindTimestamp, JetGoType: "time.Time"},
		{Kind: KindDuration, JetGoType: "int64"},
		{Kind: KindEnumAsText, JetGoType: "string"},
	} {
		assert.NoError(t, validateUniqueIndexable(&c),
			"kind %d / go %q should accept unique", c.Kind, c.JetGoType)
	}
}
