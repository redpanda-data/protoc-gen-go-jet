---
name: protoc-gen-go-jet
description: "Author Postgres migrations when a persisted proto changes, manage the protoc-gen-go-jet generation pipeline, wire new resources, and diagnose drift. The `protoc-gen-go-jet diff` subcommand either copies ddl.sql verbatim (first-ever migration) or emits an LLM brief (the caller authors the ALTER SQL beneath it); `protoc-gen-go-jet check` re-runs the whole chain and fails if it diverges from ddl.sql. Trigger when a proto with `(gojet.v1.table)` changes, a new persisted resource is added, or `protoc-gen-go-jet check` reports drift."
---

# protoc-gen-go-jet — full workflow

Proto is the API + schema declaration. The plugin turns annotated messages into a complete Postgres storage layer: DDL, jet model/table, mapper, pagination schema, aliases with embedded DDL. Migrations are authored by the human/LLM operator, scaffolded by `protoc-gen-go-jet diff` with a structured brief. `protoc-gen-go-jet check` is the machine-verified guardrail.

## Files you care about

| File | Owner | Regenerated | Lifecycle |
|---|---|---|---|
| Your annotated `*.proto` files with `(gojet.v1.table)` | Human | never | Edit during API design |
| `<proto_pkg>/storage/<resource>_ddl.sql` | **Plugin** | every `buf generate` | Canonical SQL; reference, not applied |
| `<proto_pkg>/storage/<resource>_{mapper,schema,aliases}.go` | **Plugin** | every `buf generate` | `<Prefix>Model`, `<Prefix>Table`, `<Prefix>ListSchema`, `<Prefix>DDL` (go:embed), `<Prefix>UpdateAll()` |
| `<proto_pkg>/storage/jet/public/{model,table}/*.go` | **Plugin** (internal) | every `buf generate` | Emitted by jet during plugin run |
| Your migrations directory: `NNNN_*.up.sql` | **Human** (with scaffold) | only when proto changes shape | Forward-only deploy mechanism |

One hand-written consumer tree lives next to your generated storage package: `repository.go`, `repository_pg.go`, `repository_memory.go`, `*_test.go` — they import the generated package under a `gen` alias for everything.

## When to trigger this skill

1. **Proto field added/removed/retyped** on a message with `(gojet.v1.table)`. Regenerate, scaffold a migration with `diff`, and review it.
2. **New annotated message** added. The `diff` scaffold detects the missing table and emits the CREATE.
3. **New proto package** with annotated messages (rare). No tooling changes — the plugin discovers everything from proto. You only point `--ddl-roots` at the new storage package.
4. **`protoc-gen-go-jet check` reports drift**. The failure is a path-qualified structural diff — translate it into the missing migration.

## The workflow

There is no task runner. Three explicit steps drive the whole loop: regenerate, scaffold the migration, drift-check.

```bash
# 1. Edit the proto, then regenerate everything (runs the plugin as a buf plugin).
buf generate

# 2. Pick the next sequence number yourself, scaffold the migration into a new file.
protoc-gen-go-jet diff \
    --ddl-roots=<dir-with-*_ddl.sql> \
    --migrations=<your-migrations-dir> \
    [--tenant-role=<role>] \
    > <your-migrations-dir>/NNNN_<short_snake_desc>.up.sql

# 3. Author the SQL (brief mode only — see below), then drift-check.
protoc-gen-go-jet check \
    --ddl-roots=<dir-with-*_ddl.sql> \
    --migrations=<your-migrations-dir> \
    [--tenant-role=<role>]
```

`buf generate` runs `protoc-gen-go-jet` as a buf plugin (see the repo's `buf.gen.*.yaml` templates and `cmd/protoc-gen-go-jet/e2e/buf.gen.yaml` for a worked template). One pass emits ddl.sql + mapper + schema + aliases + the go-jet model/table packages. **Docker is required** — the plugin boots an ephemeral Postgres internally to drive go-jet's schema introspection.

`--ddl-roots` is one or more comma-separated directories containing the plugin-managed `*_ddl.sql` files. `--migrations` is your forward-only migrations directory. `--tenant-role` names the runtime PG role your RLS policies attach to; drop it if the schema isn't tenant-scoped. Both `diff` and `check` boot ephemeral Postgres containers per invocation — nothing persists between runs.

### `diff` has two modes

The `diff` subcommand dispatches on whether your migrations directory already has non-empty migrations:

**Initial mode** — no non-empty migrations under the target directory. The plugin concatenates every plugin-managed `*_ddl.sql` (strip header, preserve order) into the scaffold. Applying it reproduces ddl.sql 1:1. No LLM authoring needed; `check` passes automatically.

**Brief mode** — existing migrations present. The plugin boots one ephemeral Postgres, applies the existing migration chain, dumps the resulting schema via `pg_dump --schema-only`, and writes a structured brief to stdout:

- Migration preamble (boilerplate)
- `LLM BRIEF` banner pointing back to this skill
- Active `PLUGIN-RENAME` directives (one line per rename)
- `CURRENT` section — full `pg_dump` output of the migration-chain-produced schema, SQL-commented
- `TARGET` section — concatenated `*_ddl.sql`, SQL-commented
- `TODO(llm):` marker

The LLM (or human) authors forward-only ALTER/CREATE/DROP SQL beneath the TODO marker. `check` then spins up two cold PGs, applies every migration to one and every ddl.sql to the other, and fails if any plugin-managed table diverges. That diff is the authoritative verdict — if it's clean, the authored SQL is correct.

Leaving the TODO marker in place will fail `check` because the migration body is still effectively empty relative to the schema delta.

A handy preview before writing any migration: `git diff HEAD -- '*_ddl.sql'` shows the raw SQL delta since HEAD — confirms the proto edit landed the shape you intended before a migration carries it to the schema.

## Editing the proto — the annotation checklist

```proto
import "gojet/v1/options.proto";

// Thing is a widget. Leading comments flow to COMMENT ON TABLE / COLUMN
// and into go-jet model doc comments.
message Thing {
  option (gojet.v1.table) = {
    name: "things"
    primary_key: ["tenant_id", "name"]
    indexes: [{ columns: ["tenant_id", "created_at"], order: ["ASC", "DESC"] }]
    tenancy: { column: "tenant_id", runtime_role: "app-tenant" }
    default_order_by: "created_at desc"
    tie_breaker: "name asc"
  };

  // Resource name.
  string name        = 1 [(gojet.v1.column) = { immutable: true, orderable: true, check: "name <> ''" }];
  string display     = 2 [(gojet.v1.column) = { orderable: true }];
  Type   type        = 3 [(gojet.v1.column) = { immutable: true }];  // enum → TEXT + auto CHECK
  repeated string tags = 4;                                          // persists by default; TEXT[]
  google.protobuf.Timestamp created_at = 10 [(gojet.v1.column) = { immutable: true, orderable: true }];
  google.protobuf.Timestamp updated_at = 11;                          // persists by default
  string url = 20 [(gojet.v1.column) = { skip: true }];               // computed at runtime; opt out
}
```

Rules:

- **Persist-by-default.** Every field on a `(gojet.v1.table)` message persists with inferred options. Annotate only to override inferred options or opt out with `skip: true` for *computed* fields (values derivable at runtime).
- **`skip` vs OUTPUT_ONLY are orthogonal.** `(google.api.field_behavior) = OUTPUT_ONLY` is about write-path semantics (server populates it); `skip: true` is about storage (don't store at all). `created_at` is usually both OUTPUT_ONLY and persisted.
- `orderable: true` enables cursor pagination. Non-orderable types (JSONB, bytes) fail the plugin if set.
- `immutable: true` is a hint to the repo layer. The generated `UpdateAll()` excludes these columns automatically. `(google.api.field_behavior) = IDENTIFIER` or `IMMUTABLE` auto-sets this — AIP-203 defines both as "server preserves through Update" which matches the storage contract. Keep the explicit storage-side annotation if you want defence-in-depth against an API refactor that drops the behavior annotation (the two agree and the storage side stays pinned).
- `allow_zero_enum: true` when the UNSPECIFIED value is a legitimate stored state (e.g. `CREDENTIAL_MODE_UNSPECIFIED` = "use default"). Otherwise excluded from auto-CHECK.
- `check: "..."` — arbitrary CHECK body. Overrides the auto-generated enum CHECK if both set.
- `rename_from: "old_name"` on a field declares a column rename. The plugin writes a `PLUGIN-RENAME` directive into the emitted ddl.sql; the `diff` subcommand surfaces it in the brief's "Active PLUGIN-RENAME directives" list. Author the matching `ALTER TABLE ... RENAME COLUMN` in the migration body. Drop the annotation on the next release once every environment has applied the rename.
- `unique: true` on a column auto-scopes to `(tenant_id[, user_id], col)` to match the RLS boundary. Explicit `Table.indexes` entries with `unique: true` must include the scope columns or use `where:` for a deliberately-global partial index.
- `foreign_key = { target: "<Message>.<field>" }` declares a single-column FK to a field on another persisted message. See "Foreign keys" below.
- Oneof with `(gojet.v1.oneof_column) = {}` → emits `<name>_kind TEXT` + `<name> JSONB` pair with auto CHECK on variants.
- `default_order_by` / `tie_breaker` name columns (not proto fields). Tie-breaker is usually the PK — makes pagination deterministic.
- No `output { ... }` block needed — every path is derived from the proto's Go package (`<proto_pkg>/storage`).

### Cross-tenant uniqueness guards (all caught at generate time)

- Tenant-scoped `primary_key` must include `tenancy.column` — otherwise the PK enforces uniqueness globally across tenants and a duplicate-key error at INSERT leaks the existence of another tenant's row.
- User-scoped tenancy additionally requires `tenancy.user_column` in the PK — same leak, one scope level deeper.
- `Table.indexes` entries with `unique: true` (and no `where:` partial) must include the tenant/user scope columns for the same reason.
- `skip: true` still applies: opting a field out of persistence also opts it out of every guard above.

## Authoring a migration from a brief

When the scaffold comes back in brief mode, the body you author goes directly beneath the `TODO(llm):` marker. Drift-check (`check`) catches structural mistakes but doesn't audit your intent.

**The brief has three signal sources — read all three before writing any SQL:**

1. **Active PLUGIN-RENAME directives** (top block, only present when `rename_from` was declared) — these are ground truth about authorial intent. The column WILL show up in DELTA as a `- old` + `+ new` pair because the generator diff is deliberately mechanical. It's your job to reconcile: a matching directive means write one `ALTER TABLE ... RENAME COLUMN` line instead of a DROP+ADD. A directive without a matching DELTA pair means the rename already landed; drop the annotation.
2. **DELTA** — the mechanical structural diff. Each line is `+ kind t.name`, `- kind t.name`, or `~ kind t.name`. The line tells you WHAT changed and which name, not WHY or HOW. Kinds: `column`, `check`, `index`, `policy`, plus the whole-table `primary_key`, `rls`, `table_comment`.
3. **TARGET** — full concatenated ddl.sql. This is where you look up the new column's type / default / comment / constraint body whenever DELTA says something was added or modified. Never hand-type a column definition; copy from TARGET.

**Intent inference heuristics** (the generator cannot make these calls for you):

- `-` column + `+` column, both with similar type, and a PLUGIN-RENAME directive connecting them → RENAME, not DROP+ADD. (DROP+ADD loses data.)
- `-` column without a matching `+`, and no rename directive → a real drop. Consider whether a two-stage deprecation (`skip: true` first, real DROP later) applies before emitting `ALTER TABLE t DROP COLUMN c;`.
- `~ rls t` with target disabled → STOP. RLS off means every tenant sees every row. Confirm the proto actually dropped tenancy and the drop is intentional.
- `~ primary_key t` → schedule carefully. The DROP CONSTRAINT / ADD CONSTRAINT pair leaves the table without a PK for an instant; concurrent writes during that window can lose uniqueness invariants.

**Common deltas and their idioms:**

| Change | SQL |
|---|---|
| Column added | `ALTER TABLE t ADD COLUMN c TYPE [NOT NULL DEFAULT ...];` |
| Column dropped (forward-only = destructive) | `ALTER TABLE t DROP COLUMN c;` Prefer two-stage: annotate `skip: true`, deploy, then drop. |
| Column renamed (PLUGIN-RENAME directive present) | `ALTER TABLE t RENAME COLUMN old TO new;` |
| Column type changed | `ALTER TABLE t ALTER COLUMN c TYPE new_type USING ...;` — rewrites every row under AccessExclusive on a populated table. |
| Column SET NOT NULL on populated table | `ALTER TABLE t ADD CONSTRAINT t_c_not_null CHECK (c IS NOT NULL) NOT VALID;` then `VALIDATE CONSTRAINT` in a follow-up migration. |
| CHECK added on populated table | `ALTER TABLE t ADD CONSTRAINT name CHECK (expr) NOT VALID;` then `VALIDATE CONSTRAINT` later — avoids full-table scan under AccessExclusive. |
| Index added on populated table | `CREATE INDEX CONCURRENTLY ix ON t (...);` — **but** golang-migrate wraps each file in a transaction. CONCURRENTLY only works if the file is standalone (one statement) or if you drop CONCURRENTLY and accept the brief lock. |
| Enum CHECK variant added | Drop old CHECK, add new one including the new variant. Do it in one migration; the CHECK is short-lived between statements. |
| RLS policy added | Same as ddl.sql: `CREATE POLICY n ON t TO "app-tenant" USING (...) WITH CHECK (...);` (quote the hyphenated role). |
| RLS policy body changed | `DROP POLICY n ON t; CREATE POLICY n ON t TO "..." USING (new_expr) ...;` — PG has no `ALTER POLICY USING (...)` on all versions. |
| Table added | Paste the `CREATE TABLE` from TARGET, plus its CHECKs, RLS, indexes, and COMMENT ON. |
| Table dropped | `DROP TABLE t;` — data loss; coordinate with the team. |
| COMMENT ON changed | `COMMENT ON TABLE t IS $$new body$$;` — COMMENTs don't lock; safe to batch. |

**Hazards to watch:**

- `DROP COLUMN` is irreversible and runs under AccessExclusive; the row-remove phase is fast (catalog only) but rewriting pages happens lazily — still queue behind it.
- `ALTER TABLE ... SET DEFAULT` doesn't backfill existing rows. If you need existing rows to take the new default, follow with an explicit `UPDATE`.
- `DISABLE ROW LEVEL SECURITY` is a tenancy leak — every tenant sees every row from that statement forward. Never.
- Primary-key swap: `DROP CONSTRAINT t_pkey; ADD CONSTRAINT t_pkey PRIMARY KEY (...)`. The window between the two is a window with no PK; schedule carefully.
- GIN / BRIN indexes on large tables can build for hours — always `CONCURRENTLY`.
- Stale `rename_from` (the directive points at a column that already has the new name) is an author bug. Drop the annotation and regenerate.

**Workflow — delegate to a subagent, don't author inline.**

Migration authoring is a tight mechanical loop: scaffold, edit, drift-check, read diff, edit again, drift-check again, until green. Context-heavy steps (reading a 100-line brief, staring at a cmp.Diff dump) waste the parent conversation's context window. Spawn a subagent via the Agent tool and hand it the whole loop.

Template prompt for the subagent:

```
Author a forward-only Postgres migration. You are the last-mile author
in the protoc-gen-go-jet flow.

Proto change: <one-sentence summary of what the user just edited,
e.g. "added map<string,string> labels on Widget", or "renamed
description -> summary via rename_from">.

Tools: Edit, Read, Bash, Grep.

Variables: DDL_ROOTS=<dir-with-*_ddl.sql>, MIGRATIONS=<your-migrations-dir>,
TENANT_ROLE=<role or omit>.

Procedure:
1. buf generate (skip if already done).
2. Pick the next NNNN sequence number by looking at MIGRATIONS, then:
   protoc-gen-go-jet diff --ddl-roots=$DDL_ROOTS --migrations=$MIGRATIONS \
     [--tenant-role=$TENANT_ROLE] > $MIGRATIONS/NNNN_<snake_desc>.up.sql
   This writes a scaffold containing an LLM brief.
3. Read the scaffolded file. The brief contains:
   - Active PLUGIN-RENAME directives (ground truth for renames)
   - A structural DELTA (+/-/~ column|check|index|policy, terse)
   - Concatenated TARGET ddl.sql (source of truth for shapes)
4. Write forward-only ALTER/CREATE/DROP beneath the `TODO(llm):`
   marker. Rules:
   - PLUGIN-RENAME directive + matching -/+ in DELTA => RENAME,
     never DROP+ADD (data loss).
   - A `~ check X` that DELTA shows with a "NOT VALID" body on the
     migrations side means the preceding migration added it NOT
     VALID. Add a sibling migration (next NNNN, e.g.
     NNNN_validate_X.up.sql) that issues `ALTER TABLE t VALIDATE
     CONSTRAINT X;` — both land in the same PR.
   - Copy column / policy / index definitions from TARGET verbatim
     (quote hyphenated role names like "app-tenant").
5. protoc-gen-go-jet check --ddl-roots=$DDL_ROOTS --migrations=$MIGRATIONS \
     [--tenant-role=$TENANT_ROLE]. Read the output.
6. If green, STOP. Report the migration file path(s).
7. If red, the diff output names exactly which column / check /
   index / policy / body differs. Fix — either edit the most
   recent migration, or add another sibling. Go to 5.

Up to ~5 iterations is normal. If after 5 you're still red,
stop and report the last diff output to the caller — it is
probably a constraint that pg_get_constraintdef renders
differently from ddl.sql (custom type, policy body, RLS
behavior) and needs a human judgment call.

See .claude/skills/protoc-gen-go-jet/SKILL.md for the full
pattern table (rename, drop, type-change, CHECK NOT VALID,
CONCURRENTLY-in-tx, RLS policy replacement).
```

Report back to the parent with just:
- The authored migration path(s)
- The final drift-check verdict
- Anything non-obvious about the resulting SQL the reviewer should double-check

The subagent's detailed iteration stays in its own context; the parent sees a clean summary.

**Do not** paste the brief's CURRENT/TARGET comment blocks into the migration body — they're `-- ` prefixed context, not executable. Write SQL beneath `TODO(llm):`.

## Common migration patterns

### Add a column
```bash
buf generate
protoc-gen-go-jet diff --ddl-roots=<dir> --migrations=<dir> [--tenant-role=<role>] \
    > <dir>/NNNN_add_description.up.sql
# → ALTER TABLE ... ADD COLUMN ... + COMMENT ON
```
Hand-written repos using `gen.<Prefix>UpdateAll()` pick up the new
field automatically on regen — the column list is generated, not
hand-maintained. No repo edit needed unless the field has special
handling (e.g. server-computed values).

### Rename a column (declarative)
1. In the proto, rename the field and add `rename_from`:
   ```proto
   string label = 2 [(gojet.v1.column) = { rename_from: "display_name" }];
   ```
2. `buf generate` — ddl.sql now carries a `-- PLUGIN-RENAME: <table> display_name -> label` directive.
3. `protoc-gen-go-jet diff ... > NNNN_rename_display_name.up.sql` — the brief surfaces the PLUGIN-RENAME directive under `Active PLUGIN-RENAME directives`. Author the migration as `ALTER TABLE t RENAME COLUMN display_name TO label;`.
4. Run `protoc-gen-go-jet check ...`. If it's green, commit.
5. After every environment has applied the migration, drop the `rename_from` annotation on the next release. Stale `rename_from` (matches the current column name) is rejected at generate.

If the rename also needs a value transform (e.g. `replace(display_name, '-', ' ')`), append the `UPDATE` after the RENAME in the same migration file.

### Drop a column (destructive)
Forward-only means this deletes data in prod. Prefer a two-stage deprecation (annotate `skip: true` to stop writing; deploy; drop in a later release). When you do drop, the brief's CURRENT-vs-TARGET delta tells you exactly which column disappears; author a single `ALTER TABLE t DROP COLUMN c;`.

### Stop persisting a field (non-destructive)
Annotate with `(gojet.v1.column) = { skip: true }` and regenerate. The field stays on the API surface (service layer populates it at runtime from whatever computed source replaces it) but leaves the table. Author the matching `ALTER TABLE t DROP COLUMN c;` beneath the brief's TODO marker.

### Bootstrap a new resource
Plugin emits `<resource>_ddl.sql` into the storage package. If your migrations directory is still empty, the next `diff` lands in **initial mode** and copies every ddl.sql verbatim — including the new resource. If migrations already exist, you're in **brief mode**; paste the new resource's `CREATE TABLE` (and its CHECKs, RLS, indexes, COMMENT ONs) from the brief's TARGET section into the migration body.

Storage is only half of a resource. The RPC service, authz wiring, and tenancy-scope plumbing are application-specific and live outside the plugin's territory. The end-to-end harness at `cmd/protoc-gen-go-jet/e2e/` shows the full round-trip — fixture protos under `cmd/protoc-gen-go-jet/e2e/proto/jet/e2e/v1/*.proto` and the integration tests in `cmd/protoc-gen-go-jet/e2e/e2e_integration_test.go` exercise the generated mapper + go-jet table + aipjet pagination against a Postgres testcontainer.

Field-kind inference (proto kind → SQL type) beyond the example above: see the full matrix in `cmd/protoc-gen-go-jet/README.md > Field-kind inference`.

## Foreign keys

Declare a FK at the field level — target is a proto reference, not a SQL column name:

```proto
message MCPServer {
  option (gojet.v1.table) = {
    name: "mcp_servers"
    primary_key: ["tenant_id", "id"]
    tenancy: { column: "tenant_id", runtime_role: "app-tenant" }
  };
  string id = 1 [(google.api.field_behavior) = IDENTIFIER];
  string provider_id = 2 [(gojet.v1.column) = {
    foreign_key: { target: "LLMProvider.id" }
  }];
}
```

The plugin resolves `LLMProvider.id` against the registry of messages in the current `buf generate` pass, then emits:

- `ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_provider_id_fkey FOREIGN KEY (tenant_id, provider_id) REFERENCES llm_providers (tenant_id, id) ON DELETE RESTRICT ON UPDATE NO ACTION;`
- `CREATE INDEX idx_mcp_servers_provider_id_fk ON mcp_servers (provider_id);` (auto-suppressed when another declared index already covers)
- A supplemental `UNIQUE (tenant_id, id)` on the target table, when the target's PK doesn't already cover the composite lookup.

Rules:

- **Target form.** `<Message>.<field>` in the current proto package; `.fq.pkg.Message.field` (leading dot) for cross-package. The referenced message must carry `(gojet.v1.table)` and the referenced field must be either the table's single-column primary key or carry `unique: true`.
- **Tenant auto-composite.** When both the referring and referenced tables carry `tenancy`, the FK expands to `(tenant_id, col) REFERENCES t (tenant_id, target)` so cross-tenant references are structurally impossible. One-sided tenancy (referrer has it, target doesn't, or vice-versa) is rejected at generate time — align both sides or use `Table.custom_sql` if the cross-scope reference is intentional.
- **Actions.** `on_delete` defaults to `ACTION_RESTRICT`; `on_update` defaults to `ACTION_NO_ACTION`. `SET NULL` needs `nullable: true` on the column; `SET DEFAULT` needs `default_expr`. `CASCADE` is never the default — name it explicitly with `on_delete: ACTION_CASCADE` when you want it.
- **Backing index.** Emitted automatically and auto-suppressed when the column is already the leading column of the primary key, a `Table.indexes` entry, or `unique: true` (on non-tenant tables). Set `index: false` to opt out when a predicate / expression index covers the column but the plugin can't statically see it.
- **Not in IDL: `NOT VALID`.** Proto declares end state ("this FK exists"). Whether the migration applies it one-shot or two-step is a migration concern: the diff scaffold emits a `HAZARD: ADD CONSTRAINT takes ACCESS EXCLUSIVE + full table scan` line when an FK is added to an already-populated table, with the `NOT VALID` + follow-up `VALIDATE CONSTRAINT` idiom spelled out. For brand-new tables the FK lands inline with the CREATE TABLE's first migration; no hazard.

When NOT to use the annotation:

- Composite FKs other than the tenancy auto-composite — use `Table.custom_sql`.
- Deferrable / `INITIALLY DEFERRED` constraints — `custom_sql`.
- FK into a table that isn't persisted by the plugin — `custom_sql`.
- FK into a column that isn't PK or `unique: true` (and isn't reachable via the tenant-composite path) — `custom_sql`, or promote the target to `unique: true` first.

Drift check extensions: `pg_constraint` rows of `contype='f'` are diffed via `pg_get_constraintdef()` (normalises action spellings + column ordering), `convalidated` is part of the snapshot so a forgotten `VALIDATE CONSTRAINT` surfaces as drift, and a FK without a backing btree leading on the FK column is itself reported as drift — an unindexed FK is a latent perf bug, not a config choice.

See `cmd/protoc-gen-go-jet/README.md` (foreign-key section) for the full FK option vocabulary.

## List filtering — AIP-160 is pushed into SQL

List methods take an `aip.Params` value directly. The `Filter` field is a
plain AIP-160 expression string; callers don't build jet conditions
themselves. The PG repository compiles the string into a jet
`BoolExpression` via `aipjet.FilterToCondition(filter, gen.<Prefix>FilterFields())`
and passes it to `aipjet.ExecuteWithCondition` so the cursor predicate
ANDs with the filter and pagination stays correct.

The plugin generates `<Prefix>FilterFields()` in aliases.go with
every column the translator can produce a WHERE clause for — synth
columns, JSONB, and bytea stay out. Durations bind as BIGINT
nanoseconds; accept bare ints or `duration("5m")`. Repeated-text
columns (TEXT[]) bind via containment — `zones = "us-west-1"`
compiles to `'us-west-1' = ANY(zones)`, `!=` wraps in NOT,
ordering is rejected. Adding a new proto field automatically
appears in the filter surface on regen; removing one removes it.
No hand-maintained map.

```go
// Repository
func (r *PGRepository) List(ctx context.Context, params aip.Params) (...) {
    cond, err := aipjet.FilterToCondition(params.Filter, gen.LLMProviderFilterFields())
    if err != nil { return ..., fmt.Errorf("%w: %w", storageerr.ErrInvalidInput, err) }
    // stmt := postgres.SELECT(...).FROM(...); pass cond as baseCondition:
    rows, next, err := aipjet.ExecuteWithCondition(ctx, schema, params, stmt, cond, tx)
}

// Service — encode the proto filter as AIP-160 string
filter := ""
if f := req.Msg.GetFilter(); f != nil {
    filter = aip.MakeNameFilter(f.GetNameContains())  // `name:"VALUE"`
}
res, err := repo.List(ctx, aip.Params{PageSize: ps, PageToken: t, Filter: filter})
```

Supported AIP-160 constructs today: `=`, `!=`, `<`, `<=`, `>`, `>=`,
`:` (has/substring on scalar strings, containment on TEXT[]),
`NOT`/`-`, `AND`, `OR`, parens. Per-column-type dispatch rejects
invalid combinations (e.g. `<` on bool, ordering on TEXT[]) with
`ErrUnsupportedFilter` — callers map that to InvalidArgument.
Never filter in-process after pagination: a filter that eliminates
most rows from a page looks "done" to the caller even when later
pages still have matches.

See the filter translator + security guards (LIKE metacharacter
escaping, 4 KiB filter-length cap) and the AIP-160 reference at
<https://google.aip.dev/160>. The narrow `aip.MakeNameFilter` /
`aip.ParseNameFilter` helpers are what memory repos use (they can't
build jet SQL).

## Writing Update in a hand-written PGRepository

The plugin emits `gen.<Prefix>UpdateAll()` in aliases.go — an
`UPDATE` statement pre-populated with every mutable column. Spread
into go-jet's chain:

```go
err = gen.LLMProviderUpdateAll().
    MODEL(updated).
    WHERE(gen.LLMProviderTable.Name.EQ(postgres.String(name))).
    RETURNING(gen.LLMProviderTable.AllColumns).
    QueryContext(ctx, tx, &written)
```

Never hand-maintain a `UPDATE(table.Col1, table.Col2, ...)` column
list — a new persisted proto field won't land there, and updates to
that field silently drop. The generator excludes primary-key members,
synthesized columns, and `immutable: true` fields; includes updated_at
and oneof kind/json pairs. Regen is the only thing that touches the
list.

## Commit shape

Every proto-shape change touches all three layers. Commit them together:

```
# proto + regenerated files
<proto_pkg>/thing.proto
<proto_pkg>/storage/<thing>_{ddl.sql, mapper.go, schema.go, aliases.go}
<proto_pkg>/thing.pb.go
<proto_pkg>/storage/jet/public/{model,table}/things.go

# migration (under your migrations directory)
<your-migrations-dir>/NNNN_<desc>.up.sql
```

Run your formatter over the proto + Go before committing; the generated
files carry `DO NOT EDIT.` headers and are not hand-formatted.

## Don'ts

- **Don't edit plugin output** (`<resource>_ddl.sql`, `<resource>_mapper.go`, `<resource>_schema.go`, `<resource>_aliases.go`, `jet/public/**`). `DO NOT EDIT.` headers. Next `buf generate` overwrites.
- **Don't write `.down.sql`**. Forward-only is the tool's convention — `check` only verifies the forward chain reproduces ddl.sql, and rollbacks are unsafe (data loss) and handled manually with review.
- **Don't paste the brief's CURRENT/TARGET comment blocks into the migration body as executable SQL** — they're `-- ` prefixed context, not statements. The migration body goes beneath the `TODO(llm):` marker.
- **Don't ship a migration without reading the "Hazards to watch" list.** Destructive operations (DROP COLUMN, DROP TABLE, DISABLE ROW LEVEL SECURITY) run without a prompt.
- **Don't bypass drift-check** when `check` is red. Fix the migration.
- **Don't cram multiple unrelated schema changes into one migration**. One logical change per `NNNN_*.up.sql`.

## References

- Plugin README (full options vocabulary, foreign-key section, field-kind inference matrix, what-won't-work list): `cmd/protoc-gen-go-jet/README.md`
- Plugin source: `cmd/protoc-gen-go-jet/`
- Options proto: `proto/gojet/v1/options.proto` (import as `gojet/v1/options.proto`)
- End-to-end worked example: `cmd/protoc-gen-go-jet/e2e/` — fixture protos under `cmd/protoc-gen-go-jet/e2e/proto/jet/e2e/v1/*.proto`, integration tests in `cmd/protoc-gen-go-jet/e2e/e2e_integration_test.go`, generation template in `cmd/protoc-gen-go-jet/e2e/buf.gen.yaml`.
