# protoc-gen-go-jet

Proto-first Postgres storage toolkit. One `buf generate` pass turns an
annotated proto message into every storage artifact a resource needs —
canonical DDL, go-jet model/table, proto↔model mapper, AIP pagination
schema, and a string-embedded copy of the DDL for fixtures. A
`check` subcommand verifies migrations match proto, a `diff` subcommand
emits the ALTERs to get them back in sync.

**Runtime requirement:** Docker. The plugin boots an ephemeral
Postgres container during `buf generate` to drive go-jet's
schema introspection, and `check` / `diff` spin up two more for
the drift / migration-scaffold flow. No shared dev DB involved;
containers are per-invocation. CI runners with Docker-in-Docker
or a Docker socket work unchanged.

See the [repository README](../../README.md) for installation and the
project overview.

## TL;DR

```proto
// LLMProvider is the stored representation of an LLM provider config.
message LLMProvider {
  option (gojet.v1.table) = {
    name: "llm_providers"
    primary_key: ["tenant_id", "name"]
    tenancy: { column: "tenant_id", runtime_role: "app-tenant" }
    default_order_by: "created_at desc"
    tie_breaker: "name asc"
    indexes: [{ columns: ["tenant_id", "created_at"], order: ["ASC", "DESC"] }]
  };

  // Resource name. Immutable. URL path segment.
  string name = 2 [(gojet.v1.column) = {
    immutable: true,
    orderable: true,
    check: "name <> ''"   // SQL CHECK body — no `CHECK` keyword
  }];
  string display_name = 3 [(gojet.v1.column) = { orderable: true }];
  LLMProviderType type = 4 [(gojet.v1.column) = { immutable: true }];
  // Operator-visible description. Persists by default — no annotation needed.
  string description = 15;
  // Computed at runtime from gateway base URL + provider name. Not persisted.
  string url = 11 [(gojet.v1.column) = { skip: true }];
  // Timestamps: server-set in the repo on every mutation; always persist.
  google.protobuf.Timestamp created_at = 5 [(gojet.v1.column) = {
    immutable: true, orderable: true
  }];
  google.protobuf.Timestamp updated_at = 6;
}
```

Fields persist by default. Annotate only to override inferred
options (`immutable`, `orderable`, custom name) or to opt OUT
with `skip: true` when the value is *computed* (assembled at
runtime from other state, like `url` above) rather than stored.
OUTPUT_ONLY is orthogonal to `skip` — `created_at` / `updated_at`
are OUTPUT_ONLY AND persisted; they just happen to be set by the
server rather than the client.

`buf generate` produces a single flat package with everything:

```
<your proto package>/storage/
├── llmprovider_ddl.sql       # canonical SQL — regenerated every pass
├── llmprovider_mapper.go     # LLMProviderModelFromProto / LLMProviderProtoFromModel
├── llmprovider_schema.go     # LLMProviderListSchema (aipjet)
├── llmprovider_aliases.go    # LLMProviderModel / LLMProviderTable / LLMProviderDDL (go:embed)
├── mcpserver_*.go
├── oauthprovider_*.go
└── jet/public/{model,table}/ # go-jet model + table-builder, introspected from ddl.sql
```

Everything exported uses the `<MessageName>` prefix so multiple
resources coexist in one `storage` package. Consumers import once and
reach every artifact for every resource:

```go
import gen "github.com/you/yourapp/storage" // the generated storage package

// Apply the canonical schema in-process (tests, fixtures).
if _, err := db.ExecContext(ctx, gen.LLMProviderDDL); err != nil { return err }

// Insert.
row, err := gen.LLMProviderModelFromProto(proto)
if err != nil { return err }
row.TenantID, row.CreatedAt, row.UpdatedAt = runner.TenantID(), now, now
err = gen.LLMProviderTable.
    INSERT(gen.LLMProviderTable.AllColumns).
    MODEL(row).
    RETURNING(gen.LLMProviderTable.AllColumns).
    QueryContext(ctx, tx, &inserted)
if err != nil { return err }

// Update — UpdateAll() excludes PK + synthesized + immutable columns,
// so fields marked `immutable: true` can't be mutated even if the
// caller put a different value in the proto.
row, err = gen.LLMProviderModelFromProto(nextProto)
if err != nil { return err }
row.TenantID, row.UpdatedAt = runner.TenantID(), time.Now().UTC()
err = gen.LLMProviderUpdateAll().
    MODEL(row).
    WHERE(gen.LLMProviderTable.Name.EQ(postgres.String(name))).
    RETURNING(gen.LLMProviderTable.AllColumns).
    QueryContext(ctx, tx, &updated)
if err != nil { return err }

// List with AIP-160 filter + AIP-158 cursor pagination. Filter
// errors are user-input issues — map them to InvalidArgument.
cond, err := aipjet.FilterToCondition(params.Filter, gen.LLMProviderFilterFields())
if err != nil { return fmt.Errorf("%w: %w", storageerr.ErrInvalidInput, err) }
rows, next, err := aipjet.ExecuteWithCondition(ctx, gen.LLMProviderListSchema,
    params, gen.LLMProviderTable.SELECT(...), cond, tx)
if err != nil { return err }
```

## The four things the plugin does

**1. Plugin mode (default, under `buf generate`)**

Emits all of the above in one pass. Boots an ephemeral Postgres
internally to drive go-jet's schema introspection from the just-emitted
ddl.sql — Docker is required. No separate jet-gen task.

**2. `protoc-gen-go-jet check`**

Boots two ephemeral Postgres containers. Applies every `*_ddl.sql` to
one, applies the full hand-written migration chain to the other,
introspects both (`information_schema` + `pg_catalog`), diffs the
structural snapshots per plugin-managed table. Exits 0 on match or
prints a path-qualified diff and exits non-zero on drift. Covers
columns, PKs, CHECKs, indexes, RLS, policies, and table/column
comments.

**3. `protoc-gen-go-jet diff`**

Emits a SQL migration scaffold to stdout — redirect it to your next
`NNNN_<desc>.up.sql`. Two modes, dispatched on the state of the
migrations directory:

- **Initial mode** — the migrations directory is empty (first-ever
  migration). The scaffold is the concatenated canonical `*_ddl.sql`
  files (headers stripped). Applying it reproduces the declared schema
  1:1; no hand-authoring needed.
- **Brief mode** — one or more migrations already exist. The command
  boots an ephemeral Postgres, applies the existing migration chain,
  dumps the resulting schema with `pg_dump --schema-only`, and writes a
  structured brief into the file: the CURRENT migration-chain schema, the
  TARGET concatenated ddl.sql, any active `PLUGIN-RENAME` directives, and
  a `TODO(llm):` marker. You (or an agent) author the forward-only
  ALTER/CREATE/DROP SQL beneath the marker; `check` is the validator.

Column renames are handled declaratively: annotate the renamed field
with `(gojet.v1.column) = { rename_from: "old_name" }`. The plugin
emits a `-- PLUGIN-RENAME` directive into ddl.sql, and the `diff`
brief surfaces it so you write a single `ALTER TABLE ... RENAME COLUMN`
instead of a data-destroying DROP+ADD. See "Rename a column" under
`Developer lifecycle > Patterns`.

**4. `protoc-gen-go-jet from-db <dsn> <out>`**

Escape hatch: point it at a live database and generate go-jet
types by direct introspection. For one-off debugging against a DSN
you control. Not used in the normal flow.

## Developer lifecycle

### New persisted resource

1. Annotate the message with `(gojet.v1.table)`. Tenancy +
   indexes + default order go on the message option. Every field
   persists by default — annotate a field with
   `(gojet.v1.column) = { ... }` only to override inferred
   options (immutable, orderable, custom name, etc.) or to opt
   out of persistence with `skip: true` on computed fields.
2. `buf generate` — all storage artifacts emitted.
3. `protoc-gen-go-jet diff --ddl-roots=<storage-dir> --migrations=<your-migrations-dir> [--tenant-role=<role>] > 0001_<resource>_initial.up.sql`
   — scaffolds the bootstrap migration. On an empty migrations
   directory this is initial mode: the file is the concatenated
   ddl.sql, ready to apply.
4. Hand-write the repository on top of the generated aliases. The
   end-to-end harness under `cmd/protoc-gen-go-jet/e2e/` is the
   worked example — fixture protos under
   `cmd/protoc-gen-go-jet/e2e/proto/jet/e2e/v1/` and round-trip
   integration tests in
   `cmd/protoc-gen-go-jet/e2e/e2e_integration_test.go` show the
   generated `<Prefix>Model` / `<Prefix>Table` / `<Prefix>ListSchema`
   / `<Prefix>UpdateAll()` in use against a real Postgres.

### Wiring the plugin into your build

Run the plugin as a buf plugin, after `protoc-gen-go`. A minimal
`buf.gen.yaml`:

```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-go-jet
    out: .
    opt:
      - paths=source_relative
      - repo_root=.
```

`repo_root` is the directory the emitted paths are relative to —
usually `.` (your repo root). The plugin writes each resource's
artifacts next to its proto's generated Go and runs go-jet into a
`jet/` subdirectory there.

Wrap the drift workflow however your project likes (Makefile,
Taskfile, shell script). The two commands you need:

```bash
# scaffold the next migration into a new sequence-numbered file
protoc-gen-go-jet diff \
  --ddl-roots=<dir-with-*_ddl.sql> \
  --migrations=<your-migrations-dir> \
  [--tenant-role=<role>] > migrations/NNNN_<desc>.up.sql

# verify the migration chain still reproduces ddl.sql
protoc-gen-go-jet check \
  --ddl-roots=<dir-with-*_ddl.sql> \
  --migrations=<your-migrations-dir> \
  [--tenant-role=<role>]
```

`--ddl-roots` accepts a comma-separated list, so one drift check can
cover several storage packages at once.

### Change to an existing resource

```
edit proto  →  buf generate  →  protoc-gen-go-jet diff ... > NNNN_what_changed.up.sql  →  protoc-gen-go-jet check ...
```

Preview the SQL delta before writing a migration with
`git diff HEAD -- '*_ddl.sql'` — it shows the shape change the next
`diff` will translate into ALTERs, a useful sanity check that the
proto edit landed what you intended.

The migration workflow is two steps, both touching only ephemeral
Postgres containers (nothing persists between invocations):

- `protoc-gen-go-jet diff` fills a new `.up.sql` with the scaffold /
  brief (boots one cold PG to dump the migration-chain schema).
- `protoc-gen-go-jet check` confirms parity: applies every ddl.sql to
  one cold PG and every migration to another, then diffs per-table.

Pick the sequence number yourself, or with whatever migration tool
(e.g. `golang-migrate`) your project already uses.

### Patterns — backfills and multi-step migrations

`diff` emits a single-statement ALTER for most shape
changes. Any work that needs a data step (backfill from another
column, rewrite to a new shape, populate a new NOT NULL column
from an expression) goes into the same `.up.sql` file by hand.
You own that file — the plugin won't rewrite it on the next
regenerate pass.

**Drift-check only compares the final schema.** The check pass
applies every migration in order and diffs the resulting structural
snapshot against what ddl.sql declares. Intermediate data steps
(UPDATE, DELETE, a temporary ALTER that gets undone later) don't
show up in the diff. This is what makes manual edits safe: as long
as the migration chain ends at the shape ddl.sql describes, drift
stays clean.

**Pattern: single-ALTER with a backfill UPDATE.**
For cheap backfills (seconds to run, fits in one transaction) keep
the plugin-emitted ADD COLUMN and insert an UPDATE right after it:

```sql
-- Scaffold generated by `protoc-gen-go-jet diff`.
ALTER TABLE llm_providers ADD COLUMN status TEXT NOT NULL DEFAULT '';

-- Hand-added backfill. The plugin emits ddl.sql with a non-empty
-- default so the ALTER above never rewrites existing rows; this
-- UPDATE walks them once and writes the computed value.
UPDATE llm_providers
SET status = CASE WHEN enabled THEN 'active' ELSE 'inactive' END;
```

The next `diff` pass sees this file in the migration chain, applies
it, and produces its own ALTERs against the resulting state. Nothing
forces you to re-emit the backfill.

**Pattern: nullable → UPDATE → SET NOT NULL for expensive backfills.**
When the backfill can't fit in a single UPDATE (large table, long-
running expression), split it across two migrations. The plugin's
`SET NOT NULL` hazard comment names this pattern when it fires; the
proactive version:

1. Proto declares the column with `nullable: true`.
2. First migration: `ADD COLUMN status TEXT` (no default, nullable).
3. Data migration (same .up.sql or a follow-up): batched UPDATEs
   populate `status`, e.g. via `WHERE status IS NULL LIMIT 10000`.
4. Flip the proto back to non-nullable, regenerate, run `diff`
   again. The next .up.sql contains `SET NOT NULL`;
   `pluginHazards` surfaces the "populate first" prerequisite so
   reviewers catch the case where step 3 didn't finish.

**Pattern: rename_from plus a value transform.**
`(gojet.v1.column).rename_from` handles pure renames — the `diff`
brief surfaces a `PLUGIN-RENAME` directive so you emit one
`ALTER TABLE RENAME COLUMN` instead of a DROP+ADD. If the rename
*also* needs the value to change (e.g. `name` → `display_name` with
a `replace()`), author
both steps in the same migration:

```sql
ALTER TABLE llm_providers RENAME COLUMN name TO display_name;
UPDATE llm_providers SET display_name = replace(display_name, '-', ' ');
```

Keep `rename_from: "name"` on the proto column only until the
migration has applied everywhere — stale `rename_from` annotations
are rejected at generate time.

## Options reference

All options live under `gojet.v1`:

```proto
import "gojet/v1/options.proto";
```

### `Table` (on a message)

| Field | Meaning |
|---|---|
| `name` | SQL table name. Required. |
| `primary_key` | Column-name list forming the PK. |
| `indexes` | Secondary indexes (see `Index`). |
| `tenancy` | Tenant isolation — synthesizes `tenant_id` + RLS policy. |
| `default_order_by` | Default `aipjet` order — `"created_at desc"` style. |
| `tie_breaker` | Appended to every ORDER BY for deterministic pagination. |
| `custom_sql` | Escape hatch — freeform SQL statements appended to the generated DDL. Use for triggers, BRIN/GiST indexes, materialised views, subquery CHECKs, non-tenancy GRANTs. Callers MUST keep statements idempotent (`CREATE INDEX IF NOT EXISTS`) so regen doesn't break bootstrapped DBs. Diff may bail; hand-author the migration. |
| `partition_by` | Emit `PARTITION BY <METHOD> (<cols>)` on the parent CREATE TABLE. See `Partition`. Child partitions are attached via hand-authored migrations; drift-check only compares the parent declaration. |

`output` fields (`go_dir`, `go_package_name`, `jet_*_import`,
`migration_dir`, `migration_seq`) used to live here and are now
derived from proto package layout. Proto annotations stay minimal —
every resource in a package lands in the same `storage/` Go package
with a `<MessageName>`-prefixed symbol.

### `Tenancy`

| Field | Default | Meaning |
|---|---|---|
| `column` | `tenant_id` | Synthesized tenant column name. |
| `runtime_role` | required | PG role the RLS policy attaches to. |
| `user_scoped` | false | Enable per-user RLS (e.g. OAuth tokens). |
| `user_column` | `user_id` | User column when `user_scoped`. |

### `Index`

| Field | Meaning |
|---|---|
| `columns` | Ordered column list. |
| `order` | Per-column `ASC`/`DESC`, parallel to `columns`. |
| `unique` | `CREATE UNIQUE INDEX` when true. |
| `where` | Partial index predicate (no `WHERE` keyword). |
| `name` | Custom index name. Default: `idx_<table>_<col1>_<col2>_...`. |

### `Partition`

| Field | Meaning |
|---|---|
| `method` | `PARTITION_METHOD_RANGE` / `PARTITION_METHOD_LIST` / `PARTITION_METHOD_HASH`. Required. |
| `columns` | Partition-key column list. Postgres requires every column to be a subset of `Table.primary_key`; the plugin enforces this at resolve time. |

### `Column` (on a field)

Every field on a `(gojet.v1.table)` message is persisted by
default with inferred options — no annotation needed. Annotate
with `(gojet.v1.column) = { ... }` only to override defaults
(immutable, orderable, custom name, etc.), or with
`(gojet.v1.column) = { skip: true }` to explicitly opt out
(computed fields, URLs assembled at runtime, derived state).

| Field | Default | Meaning |
|---|---|---|
| `skip` | false | When true, field is NOT persisted. Use for *computed* fields — values assembled at runtime from other state. Orthogonal to `(google.api.field_behavior) = OUTPUT_ONLY`, which is about write-path semantics (`created_at` is OUTPUT_ONLY AND persisted). Dominates every other option. |
| `name` | snake_case of proto field | Column name override. |
| `nullable` | false | When true, column allows NULL. |
| `default_expr` | inferred | SQL default expression (no `DEFAULT` keyword). |
| `check` | (auto for enum) | CHECK body (no `CHECK` keyword). |
| `storage` | auto | `JSONB_PROTO`, `ARRAY`, `TEXT_ENUM`, `JSONB_STRMAP`. |
| `orderable` | false | Include in `aipjet.Schema` for ordering/paging. |
| `immutable` | false | Signal to repos that this must be preserved through Update. `(google.api.field_behavior) = IDENTIFIER` or `IMMUTABLE` on the same field auto-sets this — the explicit annotation is redundant in that case. |
| `allow_zero_enum` | false | Include `<NAME>_UNSPECIFIED` in the enum CHECK. |
| `jsonb_indexed_paths` | `[]` | On JSONB columns, declared paths get a btree expression index (`((col->>'path'))`) and auto-register in `<Prefix>FilterFields` as `column.path` so AIP-160 dotted-path filters work. |
| `rename_from` | `""` | Column was renamed from this prior DB column name. Emits a `PLUGIN-RENAME` directive the `diff` brief surfaces so you write a RENAME instead of a DROP+ADD. |
| `jsonb_gin_index` | false | On JSONB columns, emit a GIN index using `jsonb_path_ops`. Makes `@>` / `@?` / `@@` containment queries index-eligible on any sub-field without enumerating paths. |
| `unique` | false | Emit `CREATE UNIQUE INDEX idx_<table>_<col>_unique`. Auto-scopes to match the RLS boundary: `(tenant_id, col)` on tenant-scoped tables, `(tenant_id, user_id, col)` on user-scoped tables, `(col)` on non-tenant tables. Rejected on JSONB / array / bool kinds. |
| `foreign_key` | nil | `ForeignKey { target: "<Message>.<field>", on_delete, on_update, index }`. Declares a single-column FK to a field on another persisted message. Both-sides tenancy auto-expands to `(tenant_id, col) -> (tenant_id, target)` so cross-tenant references are structurally impossible. Backing btree index emitted by default; auto-suppressed when another declared index (PK leading column, `Table.indexes` leading column, `unique: true` on non-tenant tables) already covers the column. See the "Foreign keys" section below. |
| `generated_expr` | `""` | SQL expression body for a stored generated column. Emits `<type> GENERATED ALWAYS AS (<expr>) STORED [NOT NULL]` in the CREATE TABLE; the mapper omits writes (PG rejects them) and `<Prefix>UpdateAll()` excludes the column. Mutually exclusive with `default_expr` / `unique` / `foreign_key`. Repos that INSERT generated columns must use `MutableColumns` rather than `AllColumns`. See "Picking the right option" below. |

### `OneofColumn` (on a oneof)

| Field | Default | Meaning |
|---|---|---|
| `name` | oneof's snake_case | Base column name — synthesizes `<name>_kind TEXT` + `<name> JSONB`. |
| `optional` | false | When true, the oneof may be unset. |
| `jsonb_indexed_paths` | `[]` | Paths inside the oneof's JSONB value column to index + expose as dotted filter identifiers. Same semantics as `Column.jsonb_indexed_paths`. Paths are absolute inside the JSON (no variant prefix); combine with `<name>_kind` when a query is variant-specific. |

### Picking the right option — task-oriented lookup

| If you want to… | Use | Notes |
|---|---|---|
| Enforce uniqueness on one column | `Column.unique = true` | Auto-scoped to `(tenant_id[, user_id], col)` to match the RLS boundary. Rejected on JSONB / array / bool kinds. |
| Enforce uniqueness on several columns together | `Table.indexes` + `unique: true` | Must include `tenant_id` (and `user_id` if user-scoped) unless you use `where:` for a deliberately-global partial index. |
| Enforce a per-tenant subset uniqueness (e.g. one active provider) | `Table.indexes` + `unique: true` + `where: ...` | Partial index; plugin doesn't enforce scope-column presence — caller owns the predicate. |
| Add a column-level CHECK (simple expression) | `Column.check = "..."` | Body only — no `CHECK` keyword, no `;`. |
| Add a table-level CHECK (spans columns) | `Table.custom_sql` | No native option; hand-author `ALTER TABLE ... ADD CONSTRAINT ...`. |
| Change the DB column name from the snake_case default | `Column.name = "..."` | Must be snake_case, validated against NAMEDATALEN. |
| Rename an existing column | `Column.rename_from = "old_name"` | Emits `PLUGIN-RENAME`; the `diff` brief surfaces it so you write `ALTER TABLE ... RENAME COLUMN` instead of a DROP+ADD. Drop the annotation after the rename has propagated everywhere. |
| Default value on a column | `Column.default_expr = "..."` | SQL expression body (no `DEFAULT` keyword). Strings need SQL quotes: `"'active'"`, not `"active"`. Changing it later only affects future inserts — existing rows aren't backfilled. |
| Stored computed column (e.g. `total = sum of buckets`) | `Column.generated_expr = "..."` | Body only — no `GENERATED ALWAYS AS` / parens. Renders `<type> GENERATED ALWAYS AS (<expr>) STORED`; PG computes the value on every INSERT/UPDATE. Mapper omits the column on writes; `UpdateAll()` excludes it. Mutually exclusive with `default_expr` / `unique` / `foreign_key`. Reference sibling columns by name; expressions must be immutable (PG enforces this at apply time). |
| Make a scalar nullable | proto3 `optional` + `Column.nullable = true` | Both required for scalars/enums/Duration — proto3 can't otherwise distinguish unset from zero. |
| Order a field in List responses | `Column.orderable = true` + `Table.default_order_by` | Nullable columns rejected (keyset comparator isn't NULL-aware). Bool columns rejected. |
| Preserve a column through Update | `Column.immutable = true` | Excluded from `<Prefix>UpdateAll()`. |
| Index a JSONB sub-field for fast equality lookups | `Column.jsonb_indexed_paths: ["a", "b.c"]` | Emits btree expression indexes and auto-registers dotted filter identifiers for AIP-160. Segments restricted to `[a-zA-Z0-9_]`. |
| Speed up open-ended JSONB containment queries (`@>`, `@?`, `@@`) | `Column.jsonb_gin_index = true` | GIN with `jsonb_path_ops`. |
| Persist a oneof | `Oneof` with `(gojet.v1.oneof_column)` annotation | Synthesises `<base>_kind TEXT` + `<base> JSONB`. Filter on the specific variant via the `_kind` column. |
| Emit freeform SQL the plugin doesn't support | `Table.custom_sql = ["..."]` | Triggers, materialised views, BRIN/GiST, non-tenancy GRANTs. Statements appended after CREATE TABLE; caller must keep them idempotent. `diff` may bail — hand-author the migration and re-run `protoc-gen-go-jet check`. |

## Field-kind inference

| Proto kind | SQL type | Go jet model type |
|---|---|---|
| `string` | `TEXT` | `string` |
| `bool` | `BOOLEAN` | `bool` |
| `int32` / `sint32` / `sfixed32` / `uint32` / `fixed32` | `INTEGER` | `int32` |
| `int64` / `sint64` / `sfixed64` / `uint64` / `fixed64` | `BIGINT` | `int64` |
| `float` | `REAL` | `float32` |
| `double` | `DOUBLE PRECISION` | `float64` |
| `bytes` | `BYTEA` | `[]byte` |
| enum | `TEXT` + auto CHECK | `string` |
| `google.protobuf.Timestamp` | `TIMESTAMPTZ` | `time.Time` |
| `google.protobuf.Duration` | `BIGINT` (nanoseconds) | `int64` |
| nested message | `JSONB` (protojson) | `string` |
| `repeated string` | `TEXT[]` | `pq.StringArray` |
| `repeated enum` | `TEXT[]` of names | `pq.StringArray` |
| `repeated bool` | `BOOLEAN[]` | `pq.BoolArray` |
| `repeated int32` / `sint32` / `sfixed32` / `uint32` / `fixed32` | `INTEGER[]` | `pq.Int32Array` |
| `repeated int64` / `sint64` / `sfixed64` / `uint64` / `fixed64` | `BIGINT[]` | `pq.Int64Array` |
| `repeated float` | `REAL[]` | `pq.Float32Array` |
| `repeated double` | `DOUBLE PRECISION[]` | `pq.Float64Array` |
| `repeated Timestamp` | `TIMESTAMPTZ[]` | `jettypes.TimestampArray` (via `ColumnOverrides`) |
| `repeated Duration` | `BIGINT[]` (nanoseconds) | `pq.Int64Array` |
| `repeated <message>` | `JSONB` array of protojson | `string` |
| `map<string,string>` | `JSONB` object | `string` (fast path — stdlib json on the raw map) |
| `map<K, scalar V>` | `JSONB` object | `string` (re-keyed to `map[string]V`; stdlib json round-trips scalar V) |
| `map<K, enum V>` | `JSONB` object of enum name strings | `string` |
| `map<K, message V>` | `JSONB` object | `string` (per-value protojson wrapped in `json.RawMessage`) |
| oneof (via `oneof_column`), message variants | `TEXT` kind + `JSONB` protojson | `string` + `string` |
| oneof (via `oneof_column`), scalar / enum variants | `TEXT` kind + `JSONB` scalar | `string` + `string` |

Map keys are any proto3-legal key kind: `string`, `bool`, and every
integer kind (`int32`, `sint32`, `sfixed32`, `uint32`, `fixed32`, and
their 64-bit siblings). Integer keys stringify via `strconv.Format*`
base 10 on write and parse back via `strconv.Parse*` on read; bool keys
serialise as `"true"` / `"false"`. Value kinds cover every scalar
(`string`, `bool`, `int*`, `uint*`, `float`, `double`, `bytes`), every
enum (stored as its `String()` name), and arbitrary nested messages.

`uint32` / `uint64` scalars and their repeated counterparts reinterpret
through two's complement on read/write — the jet model stores them as
int32 / int64 (what Postgres INTEGER / BIGINT hold), and the mapper
converts element-wise. Bit-exact round-trip including `math.MaxUint32`
/ `math.MaxUint64`.

Every kind gets a sensible default so `INSERT ... MODEL(row)` works
with zero-valued fields.

## Proto comments flow through

Leading comments on a persisted message become `COMMENT ON TABLE`;
leading comments on each persisted field become `COMMENT ON COLUMN`.
go-jet's generator reads those back from `pg_description` and emits
them as Go doc comments on the model struct. Three layers, one text
source.

The drift check compares comments too — if the migration doesn't emit
the `COMMENT ON` statement, drift-check fails.

## Tips and gotchas

**`oauth` → `OAuth` but `pkce` → `Pkce`.** go-jet's common-initialisms
list picks casing; `OAuth` is an explicit exception, `PKCE` is not.
If the build complains `undefined: model.Foo but have model.Bar`,
that's the cause — don't fight it.

**Timestamp columns have no default.** Set `default_expr: "now()"`
explicitly on `created_at` / `updated_at` if you want the DB to
stamp them on insert. Bucketed timestamps (`hour`, `day`, ...) leave
the default off so a missing write is an error, not a silently
inserted `now()`.

**`allow_zero_enum`.** Default excludes `<NAME>_UNSPECIFIED` from the
generated CHECK (proto3 zero = "unset"). If zero is a legitimate
persisted state, set `allow_zero_enum: true`.

**Oneof discriminator CHECK grows with variants.** Add a new oneof
variant and the plugin regenerates the `IN (...)` CHECK. The diff
subcommand translates that to a `DROP CONSTRAINT / ADD CONSTRAINT`
pair for you.

**Opt out explicitly for computed fields.** Fields persist by
default. The `url` field on `LLMProvider` is *derived* at runtime
from the gateway base URL plus the provider name — there's no
point storing it. `(gojet.v1.column) = { skip: true }` keeps it
out of the DDL. `OUTPUT_ONLY` alone doesn't trigger skip
(`created_at` is OUTPUT_ONLY and persisted); the annotations
address different concerns.

**Column order is not semantic.** `ALTER TABLE ADD COLUMN` always
appends; the drift check compares by column name, not position.

## Filtering

The generated `<Prefix>ListSchema` drives keyset pagination through
`pkg/aip/jet.ExecuteWithCondition`. To push a list-filter into the
SQL WHERE, pass the caller's AIP-160 filter expression through
`aipjet.FilterToCondition`:

```go
// The plugin emits <Prefix>FilterFields() in aliases.go — every
// column whose kind the translator can compile to SQL (scalars,
// timestamps, enums-as-text, durations-as-bigint-ns, and TEXT[]
// for repeated strings / repeated enums). JSONB, BYTEA, and
// synthesized tenant/user columns are excluded: JSONB has no
// scalar comparator, and exposing scoping columns would let a
// filter escape the implicit tenant bound.
cond, err := aipjet.FilterToCondition(params.Filter, gen.LLMProviderFilterFields())
if err != nil { return ..., fmt.Errorf("%w: %w", storageerr.ErrInvalidInput, err) }
rows, next, err := aipjet.ExecuteWithCondition(ctx, schema, params, stmt, cond, tx)
```

Supported AIP-160 constructs:

```
=, !=, <, <=, >, >=     comparisons (typed per column)
:                        has operator — substring match on string columns,
                         containment on TEXT[] columns
=, !=, :  on TEXT[]      'x' = ANY(col) / NOT (…); ordering rejected
timestamp("...")         RFC-3339 literal for TIMESTAMPTZ comparisons
duration("5m")           time.ParseDuration literal for BIGINT-ns columns
NOT expr, -expr          negation
expr AND expr, expr OR   conjunction, disjunction (OR binds tighter
                         than AND per the AIP-160 EBNF — not the prose)
(expr)                   explicit grouping
```

Reject-loud policy: unknown fields, operators not legal on the column's
Go type, wrong literal type, and syntactically invalid input all
return `aipjet.ErrUnsupportedFilter`. Callers map that to
`InvalidArgument` so the surface is "fail closed" — a filter never
silently returns the wrong rows.

Security: `:` escapes the LIKE metacharacters (`%`, `_`, `\`) before
wrapping the pattern, and the translator rejects filter expressions
above `aipjet.MaxFilterLength` (4 KiB) before handing them to the
parser.

Ref: <https://google.aip.dev/160>

## Files

- Plugin source: `cmd/protoc-gen-go-jet/`
- Options proto: `proto/gojet/v1/options.proto`
- Generated options bindings: `gen/go/gojet/v1/`
- Filter translator: `pkg/aip/jet/filter.go`
- End-to-end harness: `cmd/protoc-gen-go-jet/e2e/` — see below.

## End-to-end testing

`cmd/protoc-gen-go-jet/e2e/` is a self-contained package that
exercises the plugin against a Postgres testcontainer. The workflow
each test drives is:

1. Proto fixture under `cmd/protoc-gen-go-jet/e2e/proto/jet/e2e/v1/*.proto`
   with `(gojet.v1.*)` annotations.
2. `buf generate --template cmd/protoc-gen-go-jet/e2e/buf.gen.yaml`
   runs `protoc-gen-go` plus the in-tree `protoc-gen-go-jet`.
3. Generated artifacts land under `gen/` and are committed so CI's
   regen check asserts no drift between proto changes and the
   emitted Go / SQL.
4. On `go test -tags=integration`, `TestMain` boots a Postgres
   container, creates the tenancy role, applies every embedded
   `<Prefix>DDL` string, and grants CRUD to the tenant role.
5. Each test round-trips its concern through the generated
   mapper + go-jet table + aipjet pagination.

Regenerate with `go generate ./cmd/protoc-gen-go-jet/e2e/...`; the
step is idempotent. Tests stay Docker-free under `go test -short`
(the whole harness file is gated on the `integration` build tag).

Fixtures and what each pins:

- `scalars.proto` — every supported proto3 scalar kind
  (string/bool/int/sint/sfixed/uint/fixed at 32 and 64 bits, bytes,
  enum with and without `allow_zero_enum`). Boundary values tested.
- `times.proto` — `google.protobuf.Timestamp` + `Duration`,
  including orderable timestamp for pagination.
- `collections.proto` — repeated string / enum / message,
  `map<string,string>`, single nested message.
- `oneofs.proto` — required and optional oneofs, variant switch,
  DB-level CHECK rejecting a forged kind via raw SQL.
- `column_opts.proto` — `name` override, `default_expr`, `check`,
  `immutable`, `orderable`, `allow_zero_enum`.
- `tenancy.proto` — tenant-only and tenant+user RLS, composite PK,
  unique + partial + mixed-order indexes, pagination under RLS.
- `multi_resource.proto` — two messages in one .proto collapse into
  one Go package (flat-layout contract).
- `override/v1/override.proto` — `output.{go_dir, go_package_name,
  jet_model_import, jet_table_import}` escape hatch.
- `wkt.proto` — every well-known type the plugin hands off to the
  generic JSONB-proto path: wrappers (String/Int32/Int64/UInt32/
  UInt64/Bool/Bytes/Float/Double), Struct, Value, ListValue, Any,
  FieldMask, Empty. Presence semantics pinned where they survive
  round-trip; the one collision (Empty vs unset both serialise to
  `{}`) is explicitly documented.

## What won't work

Expectation: write a proto, it generates. This is what the plugin
refuses or can't yet express. Everything not listed here is supported.

### Rejected at generation (plugin won't emit)

**Enums**
- Enum with only the zero value declared, without `allow_zero_enum`.
  `enum X has no persistable values`.

**Repeated**
- `repeated bytes` — `pq` has no `BYTEA[]` scanner and writing one
  hasn't been worth it yet. Wrap the bytes in a nested message and
  use `repeated <message>` (JSONB array) until a caller needs it.

**Maps**
- `map<K, float>` / `map<K, double>` with bare float/double values —
  no scalar kind for floats in the map path yet. Use
  `google.protobuf.FloatValue` / `DoubleValue` as the value type;
  the wrapper rides `KindJSONBMapMessage` via protojson, which
  handles both WKTs natively. (`map<K, enum>` works directly — it
  has a dedicated `KindJSONBMapEnum` that stores each value as its
  `String()` name; no wrapper needed.)

**Nullable**
- `nullable: true` on a bare scalar / enum / Duration **without
  proto3 `optional`** — presence tracking requires proto3 optional.
- `nullable: true` on synthesized columns (oneof `<name>_kind` +
  `<name>` pair) — the oneof's presence is encoded by the paired
  JSON plus the kind discriminator; a SQL NULL on top is redundant.
  (Repeated / map / JSONB-list / JSONB-object kinds default to
  nullable now and silently accept `nullable: true` as a no-op.)

**Table-level**
- No `primary_key` — AIP-158 pagination needs one.
- `primary_key` / `index` / `default_order_by` / `tie_breaker`
  references a column that doesn't exist.
- Two proto fields whose snake_case names collide (e.g. `APIKey` +
  `ApiKey` → `api_key`) — set `name:` on one.
- Oneof `<base>_kind` or `<base>` shadows a scalar column of the
  same name.
- Two indexes with the same (explicit or auto-generated) name.
- Index `order` entries exceeding `columns` count.
- Index lists the same column twice.

**Tenancy cross-tenant uniqueness**
- `tenancy` declared but `primary_key` omits `tenancy.column` — the
  PK would enforce uniqueness globally across tenants, so a
  duplicate-key error on INSERT would leak another tenant's row.
- `tenancy` + `user_scoped: true` but `primary_key` omits
  `tenancy.user_column` — same leak, one scope level deeper.
- `Table.indexes` with `unique: true` (and no `where:`) whose
  columns omit `tenancy.column` (or `tenancy.user_column` under
  user-scoped) — same shape as the PK case, triggered at the
  index-definition surface. Column-level `unique: true` is the
  auto-scoped shorthand; `where:` opts into a deliberately-global
  partial index.
- Index with zero columns.
- `orderable: true` on `bool` / `[]byte` / JSONB / repeated / map /
  nullable — these kinds have no cursor codec or NULL-aware
  comparator, so keyset pagination would be silently broken.
- Tenancy without `runtime_role`.

**Oneof**
- Proto2 groups (proto3 doesn't have them).
- Oneof with zero variants (protoc would reject first anyway).

**Column options**
- `jsonb_indexed_paths` on a non-JSONB column.
- `rename_from` matching the current column name (dropped-stale
  annotation; drop it, or fix the target name).
- `rename_from` pointing at a column that doesn't exist on the
  migrations-side table (typo, or a cross-table rename the plugin
  doesn't support) — caught at diff time, not generate time; the
  `diff` subcommand errors with the three named fix paths.
- `storage:` override mismatched against the proto shape (e.g.
  `JSONB_STRMAP` on a repeated string, `TEXT_ENUM` on a bool) —
  the override would otherwise silently no-op.
- `skip: true` combined with `(google.api.field_behavior) =
  IDENTIFIER | IMMUTABLE` — one says "this is the identifier",
  the other says "don't store"; drop one.
- `skip: true` combined with any other column option (orderable,
  immutable, unique, check, default_expr, nullable,
  allow_zero_enum, jsonb_gin_index, jsonb_indexed_paths, name,
  rename_from, storage) — skip excludes the field from storage
  entirely, which makes other column options meaningless.
- Proto field whose resolved column name collides with a plugin-
  synthesised tenancy column (`tenant_id` / `user_id`) — the
  error names the specific fix path (rename the proto column via
  `(gojet.v1.column).name`, or change `tenancy.column` /
  `tenancy.user_column` on the Table option).
- Two proto messages in the same storage directory resolving to
  the same `Table.name` — both would emit CREATE TABLE and
  collide at apply time.

### Runtime / filter limits (plugin accepts, filter path limits)

**AIP-160 filter pushdown** (via `aipjet.FilterToCondition`)
- Filter on JSONB / BYTEA / map / oneof-kind columns at the top
  level — `<Prefix>FilterFields()` omits them. Use
  `jsonb_indexed_paths` on a JSONB column and the dotted-path
  identifier (`config.api_key_ref`) compiles to
  `config->>'api_key_ref'`, which the translator accepts.
- TEXT[] columns: only containment is supported (`=`, `!=`, `:`
  compile to `'x' = ANY(col)` and its negation). Ordering operators
  (`<`, `<=`, `>`, `>=`) are rejected — lexicographic-on-arrays
  semantics under AIP-160 are ambiguous.
- Map traversal (list traversal via `:` on TEXT[] works, see above).
- Functions other than `timestamp("...")` / `duration("...")`.
- Wildcards inside string literals (AIP-160 permits `*`, but the
  grammar currently treats `*` as a literal character — a behaviour
  swap needs opt-in design).

**Keyset pagination cursor**
- Order by a nullable column — the keyset comparator isn't
  NULL-aware; caller must COALESCE or avoid. Rejected at generation.
- Order by bool — rejected at generation.

### Sharp edges (not rejected, worth knowing)

- **Empty vs unset collapse**
  - `google.protobuf.Empty` set to `&Empty{}` vs never set → both
    serialise to `{}`, both read back as nil. Use a bool flag if
    you need presence.
  - `wrapperspb.Bytes(nil)` vs never set → pgx encodes nil `[]byte`
    as SQL NULL regardless of outer wrapper presence. Use
    `wrapperspb.Bytes([]byte{})` for explicit empty.
- **proto3 scalar presence** — without `optional`, zero and unset
  are the same bytes. Unavoidable at the proto3 layer.
- **Auto-mirrored immutability from `field_behavior`.** A field
  that carries `(google.api.field_behavior) = IDENTIFIER` or
  `IMMUTABLE` gets `immutable: true` auto-set at the storage
  layer. Convenient — but if a refactor removes the behavior
  annotation and the column block didn't carry an explicit
  `immutable: true`, the field silently flips to mutable and
  lands in the next `UpdateAll()` column list. Callers who want
  defence-in-depth here can keep the explicit
  `immutable: true` alongside the behavior annotation; the two
  agree and the storage side is pinned independently of the
  API-layer refactor.
- **Nested-message CHECK** — JSONB is opaque to the outer CHECK
  machinery. Use expression indexes + path-based filters for the
  common case; hand-author a CHECK migration for harder rules.
- **Schema evolution of JSONB payloads** — protojson preserves
  unknown fields by default. Removing a proto field leaves orphan
  JSON in existing rows; no migration required, no cleanup either.
- **Array element constraints** — no per-element CHECK on `TEXT[]`
  columns. Hand-write in a follow-up migration.
- **GIN index operator-class choice.** `jsonb_gin_index: true`
  uses `jsonb_path_ops` — smaller + faster for containment queries
  (`@>`, `@?`, `@@`) but drops support for the existence operators
  (`?`, `?&`, `?|`). If you need `jsonb ? 'key'` style queries,
  hand-author a `CREATE INDEX ... USING GIN (col)` (default
  `jsonb_ops` class) via `Table.custom_sql`.
- **TIMESTAMPTZ microsecond precision.** Postgres `TIMESTAMPTZ`
  stores microseconds, not nanoseconds. Sub-microsecond precision
  in a `google.protobuf.Timestamp` scalar field is truncated on
  write — `12:34:56.123456789` round-trips as `12:34:56.123456`.
  This is a PG-level floor, not a plugin limitation; we don't
  simulate nanosecond precision at the jet-model layer because that
  would require storing a separate `int64` nanoseconds column and
  composing on read, and no caller has a use case that cares.
  Timestamps *inside* a JSONB-serialised nested message keep full
  nanosecond resolution via protojson — the proto sits opaque,
  protojson hands back exact nanos on decode. If you need
  nanosecond-exact timestamps in a top-level field, store them as
  `int64` explicitly.
- **Repeated Timestamp lands as `TIMESTAMPTZ[]`** via the
  `jettypes.TimestampArray` named type (`[]time.Time`-shaped, with
  Scan / Value methods the plugin threads through
  `jetgen.ColumnOverrides`). SQL-native operators like `@>`,
  `unnest`, `ANY` apply directly. pq doesn't ship a TIMESTAMPTZ[]
  scanner, so the wrapper handles it — Scan parses each element
  from pq.StringArray, Value delegates to pq.Array.
- **Repeated Duration lands as `BIGINT[]` of nanoseconds.** Same
  wire format single `Duration` already uses. PG has no DURATION
  type.

## Well-known type mapping

The plugin recognises a handful of `google.protobuf.*` types and
maps them to a native SQL shape rather than the generic JSONB-proto
fallback:

| WKT | Storage | Model type |
|---|---|---|
| `Timestamp` | `TIMESTAMPTZ NOT NULL` | `time.Time` |
| `Timestamp` + `nullable:true` | `TIMESTAMPTZ` | `*time.Time` |
| `Duration` | `BIGINT NOT NULL DEFAULT 0` (nanoseconds) | `int64` |
| `StringValue` | `TEXT` (nullable) | `*string` |
| `BoolValue` | `BOOLEAN` (nullable) | `*bool` |
| `Int32Value` / `UInt32Value` | `INTEGER` (nullable) | `*int32` |
| `Int64Value` / `UInt64Value` | `BIGINT` (nullable) | `*int64` |
| `BytesValue` | `BYTEA` (nullable) | `*[]byte` |

Unsigned wrappers round-trip via two's-complement bit reinterpretation
— `UInt32Value{0xFFFFFFFF}` stores as SQL `-1` and reads back as
`UInt32Value{0xFFFFFFFF}`.

Everything else (`Any`, `Struct`, `Value`, `ListValue`, `FieldMask`,
`Empty`, `FloatValue`, `DoubleValue`, arbitrary nested messages) goes
through the generic `JSONB` + protojson path.

## Nullable columns

Set `nullable: true` on a field's `(gojet.v1.column)` option to drop
NOT NULL and emit a pointer-typed model field (`*string`, `*time.Time`,
`*[]byte`, ...). Presence is preserved end-to-end — explicit zero and
absent are distinguishable at every layer (proto struct, jet model,
SQL storage).

Rules:

- **Scalars** (string / bool / int / bytes / enum) — default to
  NOT NULL. Opt in with proto3 `optional` plus `nullable: true`.
  Without the `optional` keyword proto3 can't tell unset from zero,
  so the mapper has nothing to serialise as NULL.
- **Timestamp** — defaults to NOT NULL with no DEFAULT. Set
  `default_expr: "now()"` explicitly for `created_at` / `updated_at`
  style columns. Opt in to NULLability with `nullable: true`
  (proto3 `optional` is redundant; message pointers track presence
  natively). Produces a `*time.Time` model field when nullable.
- **Duration** — defaults to NOT NULL DEFAULT 0. Opt in with proto3
  `optional` plus `nullable: true`. Stored as BIGINT ns, `*int64` at
  the model. Without `optional` the message pointer tracks presence
  but the nanosecond value has no zero-is-unset convention; requiring
  `optional` keeps the contract uniform with other scalars.
- **Repeated / map / JSONB-list / JSONB-object kinds** — default to
  **nullable**. The nil proto slice / nil map survives to SQL NULL,
  and an explicit empty slice / empty map survives to an empty PG
  array / empty JSONB object. Nullable at the DDL, pointer-typed at
  the model (`*pq.StringArray`, `*jettypes.TimestampArray`, nullable
  JSONB), nil-preserving at the mapper in both directions. Kinds
  affected: `repeated string/enum/bool/int32/int64/float/double`,
  `repeated Timestamp`, `repeated Duration`, repeated message
  (JSONB array), map<string,T> and all other map kinds.
- **Opt out with `default_expr`** — set `default_expr: "'{}'"` (or
  whatever literal) on the proto column to restore NOT NULL DEFAULT
  `<expr>` for a repeated / map / JSONB column. Useful when you
  specifically want the legacy nil→empty flattening.
- **Single-message JSONB** (`KindJSONBProto`) — stays NOT NULL DEFAULT
  `'{}'::jsonb`. The empty-message convention is load-bearing elsewhere
  (oneof pair serialisation, unset-but-present rows); the mapper
  writes `{}` for nil and the reader skips that sentinel.
- **Oneof synthesized columns** (`<name>_kind` + `<name>`) — rejected
  as nullable. Presence is encoded by the paired JSON plus the kind
  discriminator; a SQL NULL on top would be double-presence.

Example:

```proto
message User {
  option (gojet.v1.table) = { ... };

  string id = 1;  // persists by default
  optional string display_name = 2 [(gojet.v1.column) = {nullable: true}];
  google.protobuf.Timestamp deleted_at = 3 [(gojet.v1.column) = {nullable: true}];
}
```

Repository-side, `deleted_at == nil` means not-soft-deleted;
`*user.DisplayName` is the set value or nil if never set.
