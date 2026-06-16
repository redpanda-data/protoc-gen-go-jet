# protoc-gen-go-jet — changelog

Human-curated summary of shipped capabilities, ordered by maturity
epoch. Every entry lists the user-facing surface that changed and
the commit that introduced it. Scope: everything in
`cmd/protoc-gen-go-jet/` plus the support packages
(`pkg/aip/jet`, `pkg/pgstore/jettypes`) whose behaviour the plugin
depends on.

For generation-time behaviour see [README.md](README.md).

## Column-option coverage

- `gojet.v1.column.name` — snake_case name override.
- `gojet.v1.column.nullable` — NULL allowed at SQL level
  (requires proto3 `optional` for scalars).
- `gojet.v1.column.default_expr` — raw SQL default, no DEFAULT
  keyword.
- `gojet.v1.column.check` — raw CHECK body, no CHECK keyword.
- `gojet.v1.column.storage` — explicit storage mode override
  (JSONB_PROTO, ARRAY, TEXT_ENUM, JSONB_STRMAP).
- `gojet.v1.column.orderable` — include in aipjet schema for
  keyset pagination. Rejected on nullable columns (no NULL-aware
  comparator yet), bool (no GT/LT), JSONB, arrays.
- `gojet.v1.column.immutable` — excluded from `<Prefix>UpdateAll`.
- `gojet.v1.column.allow_zero_enum` — include `<NAME>_UNSPECIFIED`
  in the auto-emitted enum CHECK.
- `gojet.v1.column.jsonb_indexed_paths` — btree expression
  indexes on JSONB sub-fields + auto-registered dotted-path
  identifiers in `<Prefix>FilterFields` for AIP-160 filters.
- `gojet.v1.column.rename_from` — declare prior DB column name;
  emits `PLUGIN-RENAME` directive, diff tool pre-applies
  `ALTER TABLE RENAME COLUMN` so pg-schema-diff doesn't emit
  DROP+ADD.
- `gojet.v1.column.jsonb_gin_index` — `CREATE INDEX USING GIN
  (col jsonb_path_ops)` for open-ended `@>` / `@?` / `@@`
  containment queries on any sub-field.
- `gojet.v1.column.unique` — single-column UNIQUE index, auto-
  scoped to match the RLS boundary: `(tenant_id, col)` on tenant-
  scoped tables, `(tenant_id, user_id, col)` on user-scoped
  tables, `(col)` on non-tenant tables. Prevents the duplicate-
  key error from leaking cross-tenant or cross-user state.
- `gojet.v1.column.foreign_key` — single-column FK to a field on
  another persisted message, expressed at the proto layer:
  `foreign_key = { target: "<Message>.<field>" }`. Both-sides
  tenancy auto-expands to `(tenant_id, col) REFERENCES t
  (tenant_id, target)` so cross-tenant references become
  structurally impossible; one-sided tenancy is rejected at
  generate time. Backing btree index emitted by default and
  auto-suppressed when an existing PK / `Table.indexes` leading
  column / `unique` already covers. Plugin injects `UNIQUE
  (tenant_id, target_col)` on the target when the PK doesn't
  already back the composite lookup. Drift check diffs
  `pg_constraint` FKs and flags unindexed FKs. Migration diff
  emits `NOT VALID` hazard scaffolds for FK adds on populated
  tables. See the README "Foreign keys" section.
- `gojet.v1.column.generated_expr` — stored generated columns.
  Renders `<type> GENERATED ALWAYS AS (<expr>) STORED [NOT NULL]`
  in CREATE TABLE; PG computes the value on every INSERT / UPDATE.
  The mapper omits the column on the proto→model path (writes
  would be rejected by PG), `<Prefix>UpdateAll()` excludes it from
  the SET list, and the model→proto path reads it like any other
  scalar. Mutually exclusive with `default_expr` (PG forbids
  DEFAULT alongside GENERATED) and with `unique` / `foreign_key`
  (out of scope — declare those on a regular column). The `;`-as-
  statement-terminator guard from `default_expr` / `check`
  applies. Repos performing INSERTs on a table with a generated
  column must use go-jet's `MutableColumns` rather than
  `AllColumns`, since `AllColumns` would attempt to write the
  generated column.
- `(google.api.field_behavior) = IDENTIFIER` or `IMMUTABLE` — auto-
  mirrors onto the storage layer as `immutable: true` so callers
  express the intent once. AIP-203 says both semantics require the
  server to preserve the value through Update — exactly the
  storage immutable contract. OUTPUT_ONLY and other field_behavior
  values do NOT imply immutable (updated_at is OUTPUT_ONLY but
  mutates on every write).
- `gojet.v1.column.skip` — explicit opt-out. Fields on a
  `(gojet.v1.table)` message are now persisted *by default*;
  callers annotate only to override inferred options or to
  opt-out via `skip: true` for *computed* fields (values assembled
  at runtime). OUTPUT_ONLY is unrelated — it's about write-path
  semantics; `created_at` is OUTPUT_ONLY AND persisted. The
  prior opt-in model silently dropped forgotten annotations —
  forgetful authors lost columns from the DDL with no feedback.
  The flipped default makes persistence decisions visible in the
  proto text: presence of `skip: true` is the only way a field
  disappears from the schema.

## Table-option coverage

- `gojet.v1.table.name` / `primary_key` / `indexes` / `tenancy`
  / `default_order_by` / `tie_breaker` / `output`.
- `gojet.v1.table.custom_sql` — escape hatch; freeform SQL
  statements appended after CREATE TABLE / indexes / RLS /
  comments. Callers MUST keep statements idempotent; diff may
  bail on shapes pg-schema-diff doesn't understand.

## Oneof-option coverage

- `gojet.v1.oneof_column.name` / `optional`.
- `gojet.v1.oneof_column.jsonb_indexed_paths` — expression
  indexes on the oneof's JSONB value column + dotted filter
  identifiers (absolute paths inside the JSON, no variant prefix;
  combine with `_kind` for variant-specific queries).

## Field-kind support matrix

- Every proto3 scalar (`string`, `bool`, `int32`/`sint32`/
  `sfixed32`/`uint32`/`fixed32`, `int64`/`sint64`/`sfixed64`/
  `uint64`/`fixed64`, `float`, `double`, `bytes`).
- `repeated` of every scalar kind → native array (`TEXT[]`,
  `BOOLEAN[]`, `INTEGER[]`, `BIGINT[]`, `REAL[]`,
  `DOUBLE PRECISION[]`). `repeated bytes` still rejected (pq
  has no `BYTEA[]` scanner; tracked).
- Enum → `TEXT` + auto CHECK constraint enumerating variant names.
- `repeated <enum>` → `TEXT[]` of names.
- `google.protobuf.Timestamp` → `TIMESTAMPTZ`.
- `google.protobuf.Duration` → `BIGINT` of nanoseconds.
- `repeated Timestamp` → `TIMESTAMPTZ[]` via `pkg/pgstore/jettypes.TimestampArray`
  custom Scan/Value threaded through `jetgen.ColumnOverrides`.
- `repeated Duration` → `BIGINT[]` of nanoseconds.
- Wrappers (`google.protobuf.StringValue` etc.) → nullable native
  scalar columns.
- `google.protobuf.Struct` / `Any` / `Empty` / `FieldMask` / `Value`
  / `ListValue` → `JSONB` via protojson.
- `google.rpc.Status` → `JSONB` via protojson.
- Nested messages (plain, inline-declared, two+ levels deep) → `JSONB`
  via protojson. `optional` message field semantics work naturally
  via proto3 pointer presence.
- `repeated <message>` → `JSONB` array of protojson.
- `map<K, V>` for every proto3-legal combination: every integer key,
  `string`, `bool`; values of every scalar, enum (stored by
  `String()` name), or message (protojson).
- `oneof` persisted as `<base>_kind TEXT` + `<base> JSONB` pair.
  Variants can be message, scalar, bytes, or enum (enum stored by
  name).
- proto3 `optional` scalars → nullable native columns.

## Generator infrastructure

- Per-resource generated files:
  - `<prefix>_aliases.go` — exports for the table, model type,
    `<Prefix>UpdateAll()`, `<Prefix>FilterFields()`,
    `<Prefix>Schema()`, embedded `<Prefix>DDL` string.
  - `<prefix>_ddl.sql` — canonical SQL schema; applied verbatim
    to ephemeral Postgres instances during jet introspection,
    drift check, and diff scaffold.
  - `<prefix>_mapper.go` — `<Prefix>ModelFromProto` /
    `<Prefix>ProtoFromModel` for every persisted resource.
  - `<prefix>_schema.go` — aipjet schema wiring for pagination.
- Proto leading comments flow to COMMENT ON TABLE / COLUMN / both
  oneof pair columns. go-jet's generator picks them up and emits
  them as Go doc comments on the model struct.
- Nested persisted annotations rejected with a dotted-path error
  (only top-level messages can carry `gojet.v1.table`).

## Input validation

Every caller-supplied string that lands in generated DDL is
either validated as a snake_case SQL identifier or properly
escaped via `pgIdent` (doubled-double-quote). Closes the SQL
injection surface and catches the more likely real-world
footguns (mixed-case names that PG silently lowercases,
leading-digit names, hyphens, stray whitespace).

- `validateSQLIdent` (snake_case `[a-z_][a-z0-9_]*`, 63-byte cap):
  Table.name, Column.name (explicit + auto-derived),
  Tenancy.column, Tenancy.user_column, OneofColumn.name,
  Index.name, rename_from.
- `validateJSONBPath` (segments restricted to `[a-zA-Z0-9_]`; no
  quotes, backslashes, control characters, empty segments,
  leading/trailing dots): every `jsonb_indexed_paths` entry. The
  segment-level ASCII-identifier cap stops a JSON key like
  `x-request-id` from producing `idx_t_c_x-request-id` — an
  invalid unquoted PG identifier that would fail to parse at apply.
  Exotic keys reach custom indexes via `Table.custom_sql`.
- `checkSynthIdentLen` catches the compound case where each input
  is short enough but the concatenated synthesis overshoots the
  63-byte PG NAMEDATALEN-1 limit: `<table>_<col>_check`
  constraints, `idx_<table>_<col>_<path>` JSONB path indexes,
  `idx_<table>_<col>_unique` / `_gin` indexes,
  `<table>_<base>_kind_valid` oneof CHECKs.
- RLS `CREATE POLICY ... TO <role>` uses `pgIdent` (doubled-
  double-quote, not Go's `%q` backslash-quote) so role names with
  special characters emit as valid PG.

Redundancy + structural guards:
- `unique: true` on the sole primary-key column rejected
  (PK already enforces and indexes uniqueness).
- `rename_from` matching the current column name rejected
  (stale annotation).
- `jsonb_indexed_paths` duplicates on one column / oneof rejected.
- Synthesised index names colliding with an explicit
  `Table.indexes` entry rejected.
- `orderable: true` on nullable columns rejected
  (no NULL-aware keyset comparator).

Silent-ignore footguns — annotations that parse but would produce
nothing without this guard:
- `(gojet.v1.table)` on a NESTED message rejected with a
  dotted-path error. Only top-level messages are resolved as
  persisted resources.
- `(gojet.v1.column)` on a field INSIDE a real oneof rejected.
  Oneof variants persist through the `<name>_kind` + `<name>`
  pair; per-variant columns aren't a thing.

Expression-field guards (values carry SQL expressions, not
statements):
- `default_expr`, `check`, `where` all reject embedded `;` with a
  message naming the field. Prevents a pasted statement from
  breaking the enclosing DDL. `custom_sql` exempt — that field
  IS multi-statement.

## Drift / migration tooling

- `protoc-gen-go-jet check --ddl-roots=... --migrations=...`
  asserts the migration chain produces a schema byte-identical to
  the plugin's ddl.sql output. Catches catalog comment drift that
  pg-schema-diff itself doesn't track.
- `protoc-gen-go-jet diff --ddl-roots=... --migrations=...`
  generates the forward-only SQL that turns the migrations-side
  schema into the ddl.sql-side schema. Plumbed through
  `protoc-gen-go-jet diff`.
- Plugin-level HAZARD annotator prepends `-- HAZARD-PLUGIN:`
  comments for destructive / risky statements pg-schema-diff emits:
  - Primary-key DROP CONSTRAINT
  - DROP COLUMN
  - ALTER COLUMN TYPE
  - CREATE UNIQUE INDEX
  - CREATE INDEX USING GIN without CONCURRENTLY
  - SET NOT NULL
  - ADD CONSTRAINT ... CHECK without NOT VALID
  - Oneof variant drops (`*_kind_valid` pattern)
  - CREATE / ALTER / DROP POLICY
  - DISABLE ROW LEVEL SECURITY (cross-tenant exposure if an
    annotation removal slips through review)
  - DROP TABLE
  - ALTER COLUMN SET DEFAULT (new default applies to future
    inserts only; names the UPDATE idiom for backfilling
    existing rows)
- Rename-directive pre-apply: the diff tool reads `PLUGIN-RENAME`
  comments out of the plugin DDL, pre-issues `ALTER TABLE ...
  RENAME COLUMN` on the migrations-side DB, and prepends the RENAME
  statements to the emitted migration so pg-schema-diff never sees
  the rename as a DROP+ADD.

## AIP-160 filter translator (`pkg/aip/jet`)

- Supports `=`, `!=`, `<`, `<=`, `>`, `>=`, `:` (substring / LIKE
  for strings, containment for TEXT[]), `AND`, `OR`, `NOT`,
  grouping parens.
- Column kinds: string, bool, integer, float, timestamp — all
  dispatched via concrete `postgres.ColumnXXX` + `XXXExpression`
  cases.
- TEXT[] columns (`repeated string`, `repeated enum` stored as
  names) bind through a dedicated `stringArrayOp` that emits
  `'x' = ANY(col)` for `=` / `:`, wraps NOT for `!=`, and rejects
  ordering — lexicographic-on-arrays is almost never what a caller
  means under AIP-160.
- `timestamp("RFC3339")`, `duration("5m")` function literals.
- `NULL` literal (`field = null` → `IS NULL`, `field != null` →
  `IS NOT NULL`). Rejected on ordering ops with a clear error.
- Dotted paths (`config.api_key_ref`) for JSONB sub-fields declared
  via `jsonb_indexed_paths` — compiles to `col->>'path'`.
- 4 KiB filter-length cap.

## AIP-158 keyset pagination (`pkg/aip/jet`)

- Cursor codecs: `StringCodec`, `BoolCodec`, `Int64Codec`
  (accepts int32), `Float64Codec` (accepts float32, rejects NaN),
  `TimestampCodec` (RFC-3339-nano, UTC-normalised).
- `directionExpr` / `equalityExpr` / `toLiteral` dispatch across
  every orderable column kind.

## Escape hatches (you are never blocked)

- `Table.custom_sql` — raw SQL statements appended to the DDL.
- `Table.indexes` — hand-author an index the shorthand options don't
  cover.
- `(gojet.v1.column) = { skip: true }` opts a *computed* field
  out of persistence. Fields persist by default under the flipped
  default; `skip: true` is the visible opt-out. OUTPUT_ONLY alone
  doesn't skip — that's about write-path, not storage.
- Hand-author the `.up.sql` under `migrations/` if the diff tool
  bails; drift check still validates the chain.

## Test coverage

- 30%+ unit coverage on the plugin package (the remaining 70% is
  testcontainer-bound drift / diff machinery, exercised via the
  e2e suite).
- 83% / 72% coverage on `pkg/aip` / `pkg/aip/jet`.
- e2e integration suite covers every ColumnKind, every AIP-160
  shape, every oneof variant, every WKT — plus
  the `ClusterLike` fixture that pins compatibility with a
  large, deeply nested real-world resource shape.
