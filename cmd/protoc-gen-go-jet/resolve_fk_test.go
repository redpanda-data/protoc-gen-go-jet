package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storagev1 "github.com/redpanda-data/protoc-gen-go-jet/gen/go/gojet/v1"
)

// Table-driven coverage for the FK IDL → plan contract. These tests
// run against the resolve_fk helpers directly, bypassing protogen.
// The full plugin-pass tests (which exercise protogen descriptors)
// are in the e2e package.

// ---------- action mapping ----------

func TestFKActionSQL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       storagev1.Action
		isDelete bool
		want     string
	}{
		{storagev1.Action_ACTION_UNSPECIFIED, true, "RESTRICT"},
		{storagev1.Action_ACTION_UNSPECIFIED, false, "NO ACTION"},
		{storagev1.Action_ACTION_RESTRICT, true, "RESTRICT"},
		{storagev1.Action_ACTION_CASCADE, true, "CASCADE"},
		{storagev1.Action_ACTION_SET_NULL, true, "SET NULL"},
		{storagev1.Action_ACTION_SET_DEFAULT, true, "SET DEFAULT"},
		{storagev1.Action_ACTION_NO_ACTION, true, "NO ACTION"},
	}
	for _, c := range cases {
		got := fkActionSQL(c.in, c.isDelete)
		assert.Equal(t, c.want, got, "fkActionSQL(%v, isDelete=%v)", c.in, c.isDelete)
	}
}

// ---------- target syntax ----------

func TestValidateFKTargetSyntax(t *testing.T) {
	t.Parallel()
	ok := []string{
		"LLMProvider.id",
		"MCPServer.id",
		".example.v1.LLMProvider.id",
		".pkg.Msg.field_name",
	}
	for _, s := range ok {
		assert.NoError(t, validateFKTargetSyntax(s), s)
	}

	bad := []struct {
		in, contains string
	}{
		{"", "is required"},
		{"noDot", "expected"},
		{"Msg.", "missing field name"},
		{"Cross.Pkg.field", "leading dot"},
		{"Msg. field", "whitespace"},
	}
	for _, b := range bad {
		err := validateFKTargetSyntax(b.in)
		require.Error(t, err, b.in)
		assert.Contains(t, err.Error(), b.contains, b.in)
	}
}

func TestSplitFKTarget(t *testing.T) {
	t.Parallel()
	// Simple name resolves to the current message's package.
	fqn, field := splitFKTarget("LLMProvider.id", "example.v1.MCPServer")
	assert.Equal(t, "example.v1.LLMProvider", fqn)
	assert.Equal(t, "id", field)

	// Fully qualified (leading dot) passes through verbatim.
	fqn, field = splitFKTarget(".other.pkg.v1.Thing.slug", "example.v1.MCPServer")
	assert.Equal(t, "other.pkg.v1.Thing", fqn)
	assert.Equal(t, "slug", field)

	// Self-FQN with no dots falls back to bare message name.
	fqn, field = splitFKTarget("A.x", "A")
	assert.Equal(t, "A", fqn)
	assert.Equal(t, "x", field)
}

// ---------- parseFKAnnotation ----------

func TestParseFKAnnotation_DefaultActions(t *testing.T) {
	t.Parallel()
	fk := &storagev1.ForeignKey{Target: "Msg.id"}
	c := &ColumnPlan{DBName: "col", NotNull: true}

	got, err := parseFKAnnotation(fk, c)
	require.NoError(t, err)
	assert.Equal(t, "Msg.id", got.RawTarget)
	assert.Equal(t, "RESTRICT", got.OnDelete)
	assert.Equal(t, "NO ACTION", got.OnUpdate)
	assert.True(t, got.BackingIndex, "default should emit a backing index")
}

func TestParseFKAnnotation_ExplicitIndexFalse(t *testing.T) {
	t.Parallel()
	fk := &storagev1.ForeignKey{Target: "Msg.id", Index: proto.Bool(false)}
	c := &ColumnPlan{DBName: "col", NotNull: true}

	got, err := parseFKAnnotation(fk, c)
	require.NoError(t, err)
	assert.False(t, got.BackingIndex, "explicit index=false must disable the backing index")
}

func TestParseFKAnnotation_SetNullRequiresNullable(t *testing.T) {
	t.Parallel()
	fk := &storagev1.ForeignKey{
		Target:   "Msg.id",
		OnDelete: storagev1.Action_ACTION_SET_NULL,
	}
	c := &ColumnPlan{DBName: "col", NotNull: true}

	_, err := parseFKAnnotation(fk, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SET NULL requires a nullable column")
}

func TestParseFKAnnotation_SetDefaultRequiresDefault(t *testing.T) {
	t.Parallel()
	fk := &storagev1.ForeignKey{
		Target:   "Msg.id",
		OnDelete: storagev1.Action_ACTION_SET_DEFAULT,
	}
	c := &ColumnPlan{DBName: "col", NotNull: false}

	_, err := parseFKAnnotation(fk, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SET DEFAULT requires `default_expr`")
}

// ---------- target eligibility & coverage ----------

func TestIsFKEligibleTarget(t *testing.T) {
	t.Parallel()
	singlePK := &ResourcePlan{
		TableName:  "llm_providers",
		PrimaryKey: []string{"id"},
	}
	compositePK := &ResourcePlan{
		TableName:  "mcp_servers",
		PrimaryKey: []string{"tenant_id", "id"},
	}
	assert.True(t, isFKEligibleTarget(singlePK, &ColumnPlan{DBName: "id"}))
	// Composite PK: id is part of it, but referencing `id` alone is not
	// eligible against a composite — uniqueness is of the tuple.
	assert.False(t, isFKEligibleTarget(compositePK, &ColumnPlan{DBName: "id"}))
	// unique: true is eligible.
	assert.True(t, isFKEligibleTarget(compositePK, &ColumnPlan{DBName: "slug", Unique: true}))
	// Neither is not eligible.
	assert.False(t, isFKEligibleTarget(compositePK, &ColumnPlan{DBName: "random"}))
}

func TestTargetPKCoversTenantComposite(t *testing.T) {
	t.Parallel()
	tenant := &TenancyPlan{Column: "tenant_id", RuntimeRole: "r"}

	// PK == (tenant_id, id) → covers.
	assert.True(t, targetPKCoversTenantComposite(&ResourcePlan{
		Tenancy:    tenant,
		PrimaryKey: []string{"tenant_id", "id"},
	}, "id"))

	// PK == (id, tenant_id) → set match, still covers.
	assert.True(t, targetPKCoversTenantComposite(&ResourcePlan{
		Tenancy:    tenant,
		PrimaryKey: []string{"id", "tenant_id"},
	}, "id"))

	// PK == (tenant_id, name) — doesn't cover the (tenant_id, id) lookup.
	assert.False(t, targetPKCoversTenantComposite(&ResourcePlan{
		Tenancy:    tenant,
		PrimaryKey: []string{"tenant_id", "name"},
	}, "id"))

	// Non-tenant target — never covers a composite.
	assert.False(t, targetPKCoversTenantComposite(&ResourcePlan{
		PrimaryKey: []string{"id"},
	}, "id"))

	// Three-col PK — wrong shape.
	assert.False(t, targetPKCoversTenantComposite(&ResourcePlan{
		Tenancy:    tenant,
		PrimaryKey: []string{"tenant_id", "id", "variant"},
	}, "id"))
}

func TestColumnAlreadyCovered(t *testing.T) {
	t.Parallel()
	// PK leading column counts.
	p := &ResourcePlan{PrimaryKey: []string{"id", "rev"}}
	assert.True(t, columnAlreadyCovered(p, "id"))
	assert.False(t, columnAlreadyCovered(p, "rev"))

	// Indexes entry leading column counts.
	p2 := &ResourcePlan{
		Indexes: []IndexPlan{{Name: "idx", Columns: []string{"foo", "bar"}}},
	}
	assert.True(t, columnAlreadyCovered(p2, "foo"))
	assert.False(t, columnAlreadyCovered(p2, "bar"))

	// Column.unique on a non-tenant table covers.
	p3 := &ResourcePlan{
		Columns: []ColumnPlan{{DBName: "slug", Unique: true}},
	}
	assert.True(t, columnAlreadyCovered(p3, "slug"))

	// Column.unique on a tenant-scoped table emits (tenant_id, slug),
	// which does NOT cover a FK on slug alone.
	p4 := &ResourcePlan{
		Tenancy: &TenancyPlan{Column: "tenant_id", RuntimeRole: "r"},
		Columns: []ColumnPlan{{DBName: "slug", Unique: true}},
	}
	assert.False(t, columnAlreadyCovered(p4, "slug"))
}

// ---------- completeFK ----------

func fkPlansHarness() (referrer, target *ResourcePlan, byFQN map[string]*ResourcePlan) {
	target = &ResourcePlan{
		ResourceFQN: "pkg.Provider",
		TableName:   "providers",
		Tenancy:     &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		PrimaryKey:  []string{"tenant_id", "id"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true},
			{DBName: "id", Kind: KindScalar, ProtoFieldName: "id"},
		},
	}
	referrer = &ResourcePlan{
		ResourceFQN: "pkg.MCPServer",
		TableName:   "mcp_servers",
		Tenancy:     &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		PrimaryKey:  []string{"tenant_id", "id"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true},
			{DBName: "id", Kind: KindScalar, ProtoFieldName: "id"},
			{
				DBName:         "provider_id",
				Kind:           KindScalar,
				NotNull:        true,
				ProtoFieldName: "provider_id",
				ForeignKey: &ForeignKeyPlan{
					RawTarget:    "Provider.id",
					OnDelete:     "RESTRICT",
					OnUpdate:     "NO ACTION",
					BackingIndex: true,
				},
			},
		},
	}
	byFQN = map[string]*ResourcePlan{
		target.ResourceFQN:   target,
		referrer.ResourceFQN: referrer,
	}
	return referrer, target, byFQN
}

func TestCompleteFK_TenantComposite_PKCovers(t *testing.T) {
	t.Parallel()
	referrer, target, byFQN := fkPlansHarness()
	c := &referrer.Columns[2]

	require.NoError(t, completeFK(referrer, c, byFQN))

	fk := c.ForeignKey
	assert.True(t, fk.TenantComposite, "both tables tenant-scoped → composite")
	assert.Equal(t, "tenant_id", fk.LocalTenantColumn)
	assert.Equal(t, "tenant_id", fk.TargetTenantColumn)
	assert.Equal(t, "providers", fk.TargetTable)
	assert.Equal(t, "id", fk.TargetColumn)
	assert.Equal(t, "mcp_servers_provider_id_fkey", fk.ConstraintName)
	assert.True(t, fk.BackingIndex)
	assert.Equal(t, "idx_mcp_servers_provider_id_fk", fk.IndexName)
	// PK already covers (tenant_id, id) — no supplemental UNIQUE.
	assert.Empty(t, target.SupplementalUniques)
}

func TestCompleteFK_TenantComposite_NeedsSupplementalUnique(t *testing.T) {
	t.Parallel()
	// Target PK is (tenant_id, name), target column id carries no
	// unique. Under tenant-composite the plugin silently injects
	// UNIQUE (tenant_id, id) to back the FK — the caller
	// doesn't need to opt in because per-tenant uniqueness is almost
	// always the intended invariant for ID-shaped fields anyway.
	referrer, target, byFQN := fkPlansHarness()
	target.PrimaryKey = []string{"tenant_id", "name"}
	target.Columns = append(target.Columns,
		ColumnPlan{DBName: "name", Kind: KindScalar, ProtoFieldName: "name"})

	c := &referrer.Columns[2]
	require.NoError(t, completeFK(referrer, c, byFQN))

	require.Len(t, target.SupplementalUniques, 1)
	uq := target.SupplementalUniques[0]
	assert.Equal(t, "uq_providers_tenant_id_id", uq.Name)
	assert.Equal(t, []string{"tenant_id", "id"}, uq.Columns)
}

func TestCompleteFK_TenantComposite_NoSupplemental_WhenUniqueAlready(t *testing.T) {
	t.Parallel()
	// Target column already carries `unique: true`, which on a
	// tenant-scoped table emits UNIQUE (tenant_id, id) via
	// renderUniqueColumnIndexes. Plugin must NOT double-inject a
	// redundant supplemental UNIQUE.
	referrer, target, byFQN := fkPlansHarness()
	target.PrimaryKey = []string{"tenant_id", "name"}
	target.Columns = append(target.Columns,
		ColumnPlan{DBName: "name", Kind: KindScalar, ProtoFieldName: "name"})
	for i := range target.Columns {
		if target.Columns[i].DBName == "id" {
			target.Columns[i].Unique = true
		}
	}

	c := &referrer.Columns[2]
	require.NoError(t, completeFK(referrer, c, byFQN))
	assert.Empty(t, target.SupplementalUniques)
}

func TestCompleteFK_NoTenancy(t *testing.T) {
	t.Parallel()
	referrer, target, byFQN := fkPlansHarness()
	referrer.Tenancy = nil
	target.Tenancy = nil
	// Adjust PKs to single-col since tenancy is gone.
	referrer.PrimaryKey = []string{"id"}
	target.PrimaryKey = []string{"id"}
	// Drop the synthesized tenant columns.
	referrer.Columns = referrer.Columns[1:]
	target.Columns = target.Columns[1:]

	c := &referrer.Columns[1] // provider_id
	require.NoError(t, completeFK(referrer, c, byFQN))
	assert.False(t, c.ForeignKey.TenantComposite)
}

func TestCompleteFK_AsymmetricTenancy(t *testing.T) {
	t.Parallel()
	referrer, _, byFQN := fkPlansHarness()
	referrer.Tenancy = nil
	referrer.PrimaryKey = []string{"id"}
	referrer.Columns = referrer.Columns[1:] // drop synthesized tenant_id

	c := &referrer.Columns[1] // provider_id
	err := completeFK(referrer, c, byFQN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asymmetric tenancy")
	assert.Contains(t, err.Error(), "mcp_servers")
	assert.Contains(t, err.Error(), "providers")
}

func TestCompleteFK_UnknownTarget(t *testing.T) {
	t.Parallel()
	referrer, _, byFQN := fkPlansHarness()
	c := &referrer.Columns[2]
	c.ForeignKey.RawTarget = "Ghost.id"

	err := completeFK(referrer, c, byFQN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found among persisted resources")
}

func TestCompleteFK_TargetFieldMissing(t *testing.T) {
	t.Parallel()
	referrer, _, byFQN := fkPlansHarness()
	c := &referrer.Columns[2]
	c.ForeignKey.RawTarget = "Provider.ghost"

	err := completeFK(referrer, c, byFQN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no persisted field named \"ghost\"")
}

func TestCompleteFK_TargetNotEligible_NoTenancy(t *testing.T) {
	t.Parallel()
	// Plain FK path: target has composite PK, target column not unique.
	referrer, target, byFQN := fkPlansHarness()
	// Demote tenancy on both sides.
	referrer.Tenancy = nil
	target.Tenancy = nil
	referrer.Columns = referrer.Columns[1:] // drop synth tenant_id
	target.Columns = target.Columns[1:]
	referrer.PrimaryKey = []string{"id"}
	target.PrimaryKey = []string{"id", "revision"}
	target.Columns = append(target.Columns,
		ColumnPlan{DBName: "revision", Kind: KindScalar, ProtoFieldName: "revision"})

	c := &referrer.Columns[1] // provider_id, now index 1 after drop
	err := completeFK(referrer, c, byFQN)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not FK-eligible")
}

func TestCompleteFK_SuppressesIndexWhenCovered(t *testing.T) {
	t.Parallel()
	referrer, target, byFQN := fkPlansHarness()
	// Make target single-col PK so eligibility passes trivially.
	target.PrimaryKey = []string{"id"}
	target.Tenancy = nil
	target.Columns = target.Columns[1:] // drop synth tenant
	referrer.Tenancy = nil
	referrer.PrimaryKey = []string{"id"}
	referrer.Columns = referrer.Columns[1:]
	// Declare an existing index whose leading column IS the FK column.
	referrer.Indexes = []IndexPlan{{Name: "idx_mcp_provider_created", Columns: []string{"provider_id", "created_at"}, Order: []string{"ASC", "ASC"}}}

	c := &referrer.Columns[1]
	require.NoError(t, completeFK(referrer, c, byFQN))
	assert.False(t, c.ForeignKey.BackingIndex, "leading column of an existing index must suppress auto-index")
	assert.Empty(t, c.ForeignKey.IndexName)
}

// ---------- emit rendering ----------

func TestRenderSchemaSQL_FK_Plain(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "mcp_servers",
		PrimaryKey: []string{"id"},
		Columns: []ColumnPlan{
			{DBName: "id", SQLType: "TEXT", NotNull: true, Kind: KindScalar, JetFieldName: "ID", JetGoType: "string"},
			{
				DBName: "provider_id", SQLType: "TEXT", NotNull: true, Kind: KindScalar,
				JetFieldName: "ProviderID", JetGoType: "string",
				ForeignKey: &ForeignKeyPlan{
					TargetTable:    "providers",
					TargetColumn:   "id",
					OnDelete:       "RESTRICT",
					OnUpdate:       "NO ACTION",
					ConstraintName: "mcp_servers_provider_id_fkey",
				},
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "CREATE TABLE mcp_servers (")
	assert.Contains(t, got, "CONSTRAINT mcp_servers_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES providers (id) ON DELETE RESTRICT ON UPDATE NO ACTION")
}

func TestRenderSchemaSQL_FK_TenantComposite(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "mcp_servers",
		PrimaryKey: []string{"tenant_id", "id"},
		Tenancy:    &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true, JetFieldName: "TenantID", JetGoType: "string"},
			{DBName: "id", SQLType: "TEXT", NotNull: true, Kind: KindScalar, JetFieldName: "ID", JetGoType: "string"},
			{
				DBName: "provider_id", SQLType: "TEXT", NotNull: true, Kind: KindScalar,
				JetFieldName: "ProviderID", JetGoType: "string",
				ForeignKey: &ForeignKeyPlan{
					TargetTable:        "providers",
					TargetColumn:       "id",
					TenantComposite:    true,
					LocalTenantColumn:  "tenant_id",
					TargetTenantColumn: "tenant_id",
					OnDelete:           "CASCADE",
					OnUpdate:           "NO ACTION",
					ConstraintName:     "mcp_servers_provider_id_fkey",
				},
			},
		},
	}
	got := renderSchemaSQL(p)
	assert.Contains(t, got, "FOREIGN KEY (tenant_id, provider_id) REFERENCES providers (tenant_id, id) ON DELETE CASCADE ON UPDATE NO ACTION")
}

func TestRenderForeignKeyIndexes(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		TableName: "t",
		Columns: []ColumnPlan{
			{DBName: "a", ForeignKey: &ForeignKeyPlan{BackingIndex: true, IndexName: "idx_t_a_fk"}},
			{DBName: "b", ForeignKey: &ForeignKeyPlan{BackingIndex: false}},
		},
	}
	var b strings.Builder
	renderForeignKeyIndexes(&b, p)
	got := b.String()
	assert.Contains(t, got, "CREATE INDEX idx_t_a_fk ON t (a);")
	assert.NotContains(t, got, "ON t (b)", "BackingIndex=false suppresses the index emit")
}

func TestRenderSupplementalUniques(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		TableName: "providers",
		SupplementalUniques: []SupplementalUniqueConstraint{
			{Name: "uq_providers_tenant_id_id", Columns: []string{"tenant_id", "id"}},
		},
	}
	var b strings.Builder
	renderSupplementalUniques(&b, p)
	got := b.String()
	assert.Contains(t, got, "CREATE UNIQUE INDEX uq_providers_tenant_id_id ON providers (tenant_id, id);")
}

func TestTopoSortDDL(t *testing.T) {
	t.Parallel()
	// Declare in reverse dep order to prove the sorter re-orders:
	// spoke references hub, so the sort must place hub first.
	files := []FileContent{
		{Path: "spoke_ddl.sql", Content: "CREATE TABLE spoke (id TEXT, hub_id TEXT, CONSTRAINT fk FOREIGN KEY (hub_id) REFERENCES hub (id) ON DELETE CASCADE ON UPDATE NO ACTION);"},
		{Path: "hub_ddl.sql", Content: "CREATE TABLE hub (id TEXT);"},
	}
	ordered, err := topoSortDDL(files)
	require.NoError(t, err)
	require.Len(t, ordered, 2)
	assert.Equal(t, "hub_ddl.sql", ordered[0].Path)
	assert.Equal(t, "spoke_ddl.sql", ordered[1].Path)
}

func TestTopoSortDDL_IgnoresExternalReferences(t *testing.T) {
	t.Parallel()
	// REFERENCES to a table that isn't created by any input file —
	// e.g. a custom_sql-declared parent or a table owned by another
	// app — is skipped, not treated as an error.
	files := []FileContent{
		{Path: "a_ddl.sql", Content: "CREATE TABLE a (id TEXT, x TEXT, CONSTRAINT fk FOREIGN KEY (x) REFERENCES external (id));"},
	}
	ordered, err := topoSortDDL(files)
	require.NoError(t, err)
	require.Len(t, ordered, 1)
	assert.Equal(t, "a_ddl.sql", ordered[0].Path)
}

func TestTopoSortDDL_DetectsCycle(t *testing.T) {
	t.Parallel()
	// Not reachable from the plugin (the resolver rejects self-FKs
	// and asymmetric tenancy, and the composite form can't form a
	// cycle through a single-column FK graph), but the defensive
	// check needs to fire loudly if the input ever reaches that
	// shape — e.g. via custom_sql authoring mistakes leaking in.
	files := []FileContent{
		{Path: "a_ddl.sql", Content: "CREATE TABLE a (id TEXT, b_id TEXT, CONSTRAINT fk FOREIGN KEY (b_id) REFERENCES b (id));"},
		{Path: "b_ddl.sql", Content: "CREATE TABLE b (id TEXT, a_id TEXT, CONSTRAINT fk FOREIGN KEY (a_id) REFERENCES a (id));"},
	}
	_, err := topoSortDDL(files)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreign-key cycle")
}

// ---------- resolveForeignKeys (cross-plan pass) ----------

func TestResolveForeignKeys_CrossPlanErrorAggregation(t *testing.T) {
	t.Parallel()
	// Two plans, two FKs — one resolves cleanly, one references an
	// unknown target. resolveForeignKeys should surface the bad one
	// without bailing before it has looked at the good one.
	referrer, target, _ := fkPlansHarness()
	bad := &ResourcePlan{
		ResourceFQN: "pkg.Broken",
		TableName:   "broken",
		Tenancy:     &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		PrimaryKey:  []string{"tenant_id", "id"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", Kind: KindSynthesized, Synthesized: true},
			{DBName: "id", Kind: KindScalar, ProtoFieldName: "id"},
			{
				DBName:         "ghost_id",
				Kind:           KindScalar,
				NotNull:        true,
				ProtoFieldName: "ghost_id",
				ForeignKey: &ForeignKeyPlan{
					RawTarget: "Nope.id",
					OnDelete:  "RESTRICT",
					OnUpdate:  "NO ACTION",
				},
			},
		},
	}

	err := resolveForeignKeys([]*ResourcePlan{referrer, target, bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Nope")
	// The good FK's side effects still landed.
	fk := referrer.Columns[2].ForeignKey
	assert.Equal(t, "providers", fk.TargetTable)
}

// ---------- end-to-end renderSchemaSQL with FK ----------

func TestRenderSchemaSQL_WithForeignKey(t *testing.T) {
	t.Parallel()
	p := &ResourcePlan{
		ProtoFile:  "fixture.proto",
		TableName:  "mcp_servers",
		PrimaryKey: []string{"tenant_id", "id"},
		Tenancy:    &TenancyPlan{Column: "tenant_id", RuntimeRole: "app-tenant"},
		Columns: []ColumnPlan{
			{DBName: "tenant_id", SQLType: "TEXT", NotNull: true, Kind: KindSynthesized, Synthesized: true},
			{DBName: "id", SQLType: "TEXT", NotNull: true, Kind: KindScalar, JetFieldName: "ID", JetGoType: "string"},
			{
				DBName: "provider_id", SQLType: "TEXT", NotNull: true, Kind: KindScalar,
				JetFieldName: "ProviderID", JetGoType: "string",
				ForeignKey: &ForeignKeyPlan{
					RawTarget:          "LLMProvider.id",
					TargetTable:        "llm_providers",
					TargetColumn:       "id",
					TenantComposite:    true,
					LocalTenantColumn:  "tenant_id",
					TargetTenantColumn: "tenant_id",
					OnDelete:           "RESTRICT",
					OnUpdate:           "NO ACTION",
					BackingIndex:       true,
					ConstraintName:     "mcp_servers_provider_id_fkey",
					IndexName:          "idx_mcp_servers_provider_id_fk",
				},
			},
		},
	}
	got := renderSchemaSQL(p)

	// FK is inlined in CREATE TABLE — the constraint line sits
	// between the PK and the closing `);`. Backing index is a
	// separate CREATE INDEX further down. Both must be present.
	createIdx := strings.Index(got, "CREATE TABLE mcp_servers (")
	closeIdx := strings.Index(got[createIdx:], ");")
	require.GreaterOrEqual(t, createIdx, 0)
	require.GreaterOrEqual(t, closeIdx, 0)
	createBlock := got[createIdx : createIdx+closeIdx+2]
	assert.Contains(t, createBlock,
		"CONSTRAINT mcp_servers_provider_id_fkey FOREIGN KEY (tenant_id, provider_id) REFERENCES llm_providers (tenant_id, id)")
	// Backing index lives outside CREATE TABLE.
	assert.Contains(t, got, "CREATE INDEX idx_mcp_servers_provider_id_fk ON mcp_servers (provider_id);")
}
