package main

import (
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ResourcePlan is the fully-resolved generation plan for one annotated
// proto message.
type ResourcePlan struct {
	// Source of truth — the annotated proto message.
	Message     *protogen.Message
	ProtoFile   string
	ResourceFQN string // Proto message FQN, e.g. "example.v1.LLMProvider". Emitted as a `-- Resource:` header in ddl.sql so drift-check can filter by resource without loading descriptors.

	// Table-level
	TableName    string
	TableComment string // leading proto comment on the message, surfaced as COMMENT ON TABLE
	PrimaryKey   []string
	Indexes      []IndexPlan
	Tenancy      *TenancyPlan
	Partition    *PartitionPlan
	DefaultOrder []OrderPart
	TieBreaker   []OrderPart

	// Columns (explicit + synthesized tenant_id / user_id).
	Columns      []ColumnPlan
	OneofColumns []OneofColumnPlan

	// CustomSQL is the list of caller-supplied SQL statements appended
	// to the generated ddl.sql. Escape hatch for shapes the plugin
	// can't express declaratively (triggers, materialised views,
	// subquery-based CHECKs, non-tenancy GRANTs). Each entry is one
	// statement; a trailing semicolon is added if missing.
	CustomSQL []string

	// SupplementalUniques holds UNIQUE constraints the plugin auto-
	// emits on THIS table to back incoming tenant-composite foreign
	// keys whose target column is not already covered by the primary
	// key. Populated in the post-resolve FK pass; empty otherwise.
	SupplementalUniques []SupplementalUniqueConstraint

	// Output paths
	GoPackageName  string // shared package name, typically "storage"
	GoDir          string // directory the plugin writes into (repo-relative)
	GoImportPath   string // Go import path of the generated storage package
	FilenamePrefix string // per-resource filename prefix — "llmprovider", "mcpserver", ...
	SymbolPrefix   string // per-resource Go symbol prefix — "LLMProvider", "MCPServer", ...
	JetModelImport string
	JetTableImport string

	// Derived
	JetStructName string // e.g. "LlmProviders"
	RepoRoot      string

	// Proto Go identifier used by the generated mapper and schema.
	ProtoGoName  string // e.g. "LLMProvider"
	ProtoGoIdent protogen.GoIdent
}

// ColumnPlan describes a single SQL column plus how to map it to/from
// proto.
type ColumnPlan struct {
	// SQL
	DBName      string
	SQLType     string
	NotNull     bool
	DefaultExpr string
	Check       string
	Comment     string // leading proto comment on the field, surfaced as COMMENT ON COLUMN

	// Mapping
	JetFieldName string // e.g. "DisplayName" — go-jet model field
	JetGoType    string // e.g. "string", "time.Time", "[]byte"
	JetGoImport  string // import path for JetGoType (empty if stdlib/builtin)

	// Proto source (nil when Synthesized). Field.Desc gives source
	// location for precise error messages during emit.
	Field *protogen.Field

	// ProtoFieldName mirrors Field.Desc.Name() as a plain string. The
	// cross-plan FK resolver uses it to match an FK target like
	// `Msg.id` to a referent column without re-entering protogen.
	// Keeping it as a separate string also makes unit tests easy to
	// write — they can set ProtoFieldName without constructing a
	// synthetic protogen.Field.
	ProtoFieldName string

	// Options
	Kind      ColumnKind
	Orderable bool
	Immutable bool

	// Nullable is true when the column omits NOT NULL in the DDL. The
	// jet model renders nullable scalars as pointer types (*string,
	// *time.Time, *int64, ...); the mapper inspects this flag to emit
	// presence-preserving pointer dereferences. Bytes stays non-pointer
	// because []byte already distinguishes nil from empty natively.
	Nullable bool

	// WrapperCtor, when non-empty, names the wrapperspb constructor
	// used to re-wrap the scalar value on the proto-from-model path
	// (e.g. "String" for google.protobuf.StringValue). Wrappers are
	// persisted as nullable native columns; the mapper reads the
	// inner scalar via <ptr>.GetValue() and reconstructs the wrapper
	// message on egress. Empty means the field is not a wrapper.
	WrapperCtor string

	// Synthesized columns (tenant_id, user_id) have no proto field.
	Synthesized bool

	// MapKeyKind / MapValueKind are populated for map<K,V> fields
	// (Kind ∈ {KindJSONBStrMap, KindJSONBMapScalar, KindJSONBMapMessage}).
	// The mapper reads these to emit the right key-stringification and
	// value-encoding path. Zero for non-map columns.
	MapKeyKind   protoreflect.Kind
	MapValueKind protoreflect.Kind

	// JSONBIndexedPaths is the list of JSON paths to emit per-path
	// expression indexes for. Only populated on JSONB-backed columns.
	// Each entry is a top-level key ("api_key_ref") or a dotted path
	// ("config.timeout"). Nil for non-JSONB columns.
	JSONBIndexedPaths []string

	// RenameFrom, when set, declares the previous DB column name a
	// caller used before this field was renamed. The plugin surfaces
	// it in the DDL as a PLUGIN-RENAME directive so migration authors
	// know to write `ALTER TABLE t RENAME COLUMN <old> TO <new>`
	// instead of the DROP+ADD pair pg-schema-diff would otherwise
	// produce.
	RenameFrom string

	// JSONBGinIndex emits a GIN expression index on a JSONB-backed
	// column — makes `@>` / `@?` / `@@` queries index-eligible
	// without enumerating sub-paths. Only valid on JSONB kinds.
	JSONBGinIndex bool

	// Unique emits a one-column unique index on this column. Shorthand
	// for a `Table.indexes` entry with `unique: true` and a single
	// column. Rejected on kinds PG can't index as a plain btree:
	// JSONB / TEXT[] / repeated primitive arrays. On tenant-scoped
	// tables the plugin prepends `tenant_id` so uniqueness scopes to
	// the tenant, not the cluster.
	Unique bool

	// ForeignKey, when non-nil, declares a single-column FK reference
	// to a field on another persisted message. Populated during
	// resolve with the raw target string + actions; cross-message
	// resolution (target -> table+column) runs as a post-pass once
	// every plan in the current generation run is known.
	ForeignKey *ForeignKeyPlan

	// GeneratedExpr, when non-empty, is the SQL expression body for a
	// stored generated column. The DDL emitter renders the column as
	// `<type> GENERATED ALWAYS AS (<expr>) STORED [NOT NULL]`; the
	// mapper omits writes (PG rejects them) but reads the value back
	// like any other scalar; aliases.UpdateAll() excludes the column
	// from the UPDATE SET list.
	GeneratedExpr string
}

// ForeignKeyPlan describes a resolved foreign-key constraint on a
// ColumnPlan. The plugin emits one ALTER TABLE ADD CONSTRAINT per FK
// in a trailing section of the referring table's ddl.sql so cross-
// table references do not impose a CREATE TABLE ordering requirement
// on the applier.
type ForeignKeyPlan struct {
	// RawTarget is the proto-field reference exactly as written in
	// the annotation ("LLMProvider.id" or ".fq.pkg.Msg.field"). Kept
	// for diagnostics so errors reprint what the author typed.
	RawTarget string

	// TargetTable is the SQL table name of the referenced message,
	// filled in by the cross-plan FK resolver.
	TargetTable string

	// TargetColumn is the SQL column name of the referenced field.
	TargetColumn string

	// TenantComposite is set when both referring and referenced
	// tables carry tenancy. The emitted FK expands to
	//   FOREIGN KEY (tenant_id, <col>) REFERENCES <t> (tenant_id, <ref>)
	// which makes cross-tenant references structurally impossible.
	TenantComposite bool

	// LocalTenantColumn / TargetTenantColumn are the tenancy column
	// names of the referring and referenced tables. Both honor the
	// `tenancy.column` override and are only populated when
	// TenantComposite is true.
	LocalTenantColumn  string
	TargetTenantColumn string

	// OnDelete / OnUpdate are the canonical Postgres clause bodies
	// ("RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT", "NO ACTION").
	// Always non-empty at emit time — unspecified actions are
	// resolved to their defaults during parse.
	OnDelete string
	OnUpdate string

	// BackingIndex reports whether the plugin emits a btree index on
	// the referring column. True unless the caller sets `index: false`
	// or an already-declared index (primary key leading column,
	// `Table.indexes` entry leading column, or `unique: true`)
	// already covers the column.
	BackingIndex bool

	// ConstraintName is the auto-generated FK name,
	// `<referring_table>_<col>_fkey`. Matches the Postgres default
	// so a custom_sql FK with the default name produces a no-op
	// diff when converted to the annotation.
	ConstraintName string

	// IndexName is populated when BackingIndex is true. Follows the
	// `idx_<table>_<col>_fk` pattern — distinct from the `_unique`
	// suffix used by Column.unique so the two never collide.
	IndexName string
}

// SupplementalUniqueConstraint is an auto-emitted UNIQUE constraint
// injected onto a target table so an incoming tenant-composite
// foreign key has a non-deferrable unique constraint to reference.
// Postgres requires the referenced columns of an FK to exactly match
// some unique/PK constraint; when the target's PK doesn't already
// cover `(tenant_id, ref_col)`, the plugin synthesises one.
type SupplementalUniqueConstraint struct {
	// Name is the auto-generated constraint name, stable across
	// regenerations: `uq_<table>_<col1>_<col2>`.
	Name string
	// Columns are the constraint's columns in declared order. Always
	// two entries today: (tenant_id, target_col).
	Columns []string
}

// ColumnKind tells emit_mapper how to translate a column.
type ColumnKind int

const (
	KindScalar            ColumnKind = iota // string, bool, int32, int64, bytes
	KindTimestamp                           // google.protobuf.Timestamp <-> time.Time
	KindDuration                            // google.protobuf.Duration <-> BIGINT nanoseconds
	KindEnumAsText                          // proto enum <-> string
	KindRepeatedText                        // repeated string <-> []string (TEXT[])
	KindRepeatedEnum                        // repeated enum <-> TEXT[] of enum names
	KindRepeatedBool                        // repeated bool <-> BOOLEAN[] via pq.BoolArray
	KindRepeatedInt32                       // repeated int32/sint32/sfixed32/uint32/fixed32 <-> INTEGER[] via pq.Int32Array (uints reinterpret via two's complement)
	KindRepeatedInt64                       // repeated int64/sint64/sfixed64/uint64/fixed64 <-> BIGINT[] via pq.Int64Array
	KindRepeatedFloat32                     // repeated float <-> REAL[] via pq.Float32Array
	KindRepeatedFloat64                     // repeated double <-> DOUBLE PRECISION[] via pq.Float64Array
	KindRepeatedTimestamp                   // repeated google.protobuf.Timestamp <-> BIGINT[] of unix nanoseconds via pq.Int64Array
	KindRepeatedDuration                    // repeated google.protobuf.Duration <-> BIGINT[] of nanoseconds via pq.Int64Array
	KindJSONBProto                          // single nested message <-> JSONB via protojson
	KindJSONBProtoList                      // repeated nested message <-> JSONB array
	KindJSONBStrMap                         // map<string,string> <-> JSONB object (fast path via json.Marshal)
	KindJSONBMapScalar                      // map<K, scalar V> <-> JSONB object (stdlib json handles text-marshaler keys + scalar values)
	KindJSONBMapEnum                        // map<K, enum V> <-> JSONB object; each value is the enum's String() name
	KindJSONBMapMessage                     // map<K, message V> <-> JSONB object (per-value protojson wrapped in json.RawMessage)
	KindSynthesized                         // tenant_id, user_id (storage-owned, not proto-mapped)
)

// String returns a stable human-readable name for a ColumnKind.
// Used in caller-facing errors so messages read "got kind
// `repeated <string>`" instead of "got kind 6". Keep the mapping
// in sync with the iota block above.
func (k ColumnKind) String() string {
	switch k {
	case KindScalar:
		return "scalar"
	case KindTimestamp:
		return "timestamp"
	case KindDuration:
		return "duration"
	case KindEnumAsText:
		return "enum"
	case KindRepeatedText:
		return "repeated <string>"
	case KindRepeatedEnum:
		return "repeated <enum>"
	case KindRepeatedBool:
		return "repeated <bool>"
	case KindRepeatedInt32:
		return "repeated <int32>"
	case KindRepeatedInt64:
		return "repeated <int64>"
	case KindRepeatedFloat32:
		return "repeated <float>"
	case KindRepeatedFloat64:
		return "repeated <double>"
	case KindRepeatedTimestamp:
		return "repeated <timestamp>"
	case KindRepeatedDuration:
		return "repeated <duration>"
	case KindJSONBProto:
		return "nested message (JSONB)"
	case KindJSONBProtoList:
		return "repeated nested message (JSONB)"
	case KindJSONBStrMap:
		return "map<string,string> (JSONB)"
	case KindJSONBMapScalar:
		return "map with scalar value (JSONB)"
	case KindJSONBMapEnum:
		return "map with enum value (JSONB)"
	case KindJSONBMapMessage:
		return "map with message value (JSONB)"
	case KindSynthesized:
		return "synthesized (tenancy)"
	}
	return fmt.Sprintf("ColumnKind(%d)", int(k))
}

// OneofColumnPlan describes a persisted proto oneof.
type OneofColumnPlan struct {
	// Base SQL name
	BaseName   string // "provider_config"
	KindColumn string // "provider_config_kind"
	JSONColumn string // "provider_config" (stores protojson of the variant)

	// Leading proto comment on the oneof declaration — surfaced as
	// COMMENT ON COLUMN on both the kind discriminator and the JSON
	// value columns so psql \d+ / pgAdmin show the oneof's purpose on
	// whichever column the reader happens to inspect.
	Comment string

	Optional bool

	// Proto oneof
	Oneof    *protogen.Oneof
	Variants []OneofVariant

	// go-jet model field names for the pair
	JetKindField string // e.g. "ProviderConfigKind"
	JetJSONField string // e.g. "ProviderConfig"

	// JSONBIndexedPaths is the list of paths inside the JSON column
	// to build per-path btree expression indexes on. Each entry is a
	// dotted path rooted at the JSON column (not the _kind column),
	// e.g. "aws.accountId" compiles to `((col->'aws'->>'accountId'))`.
	// Nil means no indexes declared.
	JSONBIndexedPaths []string
}

// OneofVariant is a single case of a persisted oneof.
type OneofVariant struct {
	// snake_case name used in the kind discriminator column (e.g. "openai_config").
	VariantName string

	// Proto field reference for this variant. The wrapped message type is
	// reachable via Field.Message; callers access it there rather than
	// duplicating the reference.
	Field *protogen.Field
}

// IndexPlan describes a secondary SQL index.
type IndexPlan struct {
	Name    string
	Columns []string
	Order   []string // per-column "ASC"/"DESC", parallel to Columns
	Unique  bool
	Where   string
}

// PartitionPlan describes a Postgres PARTITION BY clause on the parent
// table. Partition columns are a subset of the primary key; Postgres
// rejects the CREATE TABLE otherwise.
type PartitionPlan struct {
	Method  string // "RANGE" | "LIST" | "HASH" — rendered verbatim.
	Columns []string
}

// TenancyPlan describes tenant/user isolation columns and RLS policy.
type TenancyPlan struct {
	Column      string
	RuntimeRole string
	UserScoped  bool
	UserColumn  string
}

// OrderPart is one (field, direction) entry from default_order_by or
// tie_breaker.
type OrderPart struct {
	FieldPath string // aipjet field path (matches a Column's aipjet key)
	Desc      bool
}
