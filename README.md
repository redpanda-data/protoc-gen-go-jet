# protoc-gen-go-jet

Proto-first PostgreSQL storage codegen for Go. You annotate a proto message and
run `buf generate`. The plugin then generates the storage layer for that
resource:

- canonical DDL (`CREATE TABLE`, CHECKs, indexes, RLS policies, comments)
- a proto/model mapper
- [go-jet](https://github.com/go-jet/jet) model and type-safe SQL builder packages
- an AIP-158 keyset-pagination schema and AIP-160 filter translator
- a string-embedded copy of the DDL for fixtures and tests

There are two more subcommands for migrations. `check` verifies that your
hand-written migrations still produce the schema your proto declares, and `diff`
scaffolds the next migration. The plugin owns the schema declaration. You own
the forward-only migrations.

## Why

Resource-oriented APIs are quick to write and hard to keep consistent. Resource
names, List/Get semantics, filters, and storage tend to drift apart over time,
and that gets worse when a coding agent writes most of the code. This plugin
ties all of that to one source of truth: the annotated proto is the schema
declaration. The mapper, go-jet types, AIP-158/160 surface, and the `check`
drift gate are generated from it, so the proto is the thing you review and the
agent targets while the generator handles what comes after.

The pattern isn't specific to Postgres. This plugin is just the Postgres +
go-jet implementation of it.

## Install

```bash
go install github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet@latest
```

Requirements: [buf](https://buf.build), Go, and Docker. During generation the
plugin boots an ephemeral Postgres container to drive go-jet's schema
introspection, plus two more for the `check` / `diff` drift flow. Containers are
per-invocation. There's no shared dev DB.

## Quick start

Annotate a message with the `gojet.v1` options:

```proto
import "gojet/v1/options.proto";

// LLMProvider is the stored representation of a provider config.
message LLMProvider {
  option (gojet.v1.table) = {
    name: "llm_providers"
    primary_key: ["tenant_id", "name"]
    tenancy: { column: "tenant_id", runtime_role: "app-tenant" }
    default_order_by: "created_at desc"
    tie_breaker: "name asc"
  };

  // Fields persist by default; annotate only to override or opt out.
  string name = 2 [(gojet.v1.column) = { immutable: true, orderable: true }];
  string display_name = 3 [(gojet.v1.column) = { orderable: true }];
  string url = 11 [(gojet.v1.column) = { skip: true }]; // computed at runtime
  google.protobuf.Timestamp created_at = 5 [(gojet.v1.column) = {
    immutable: true, orderable: true
  }];
}
```

Wire the plugin into `buf.gen.yaml`, after `protoc-gen-go`:

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

`buf generate` emits the full storage layer. After that, drive migrations with
the two subcommands:

```bash
# scaffold the next migration
protoc-gen-go-jet diff \
  --ddl-roots=<dir-with-*_ddl.sql> \
  --migrations=<your-migrations-dir> > migrations/NNNN_change.up.sql

# verify the migration chain still reproduces the declared schema
protoc-gen-go-jet check \
  --ddl-roots=<dir-with-*_ddl.sql> \
  --migrations=<your-migrations-dir>
```

Run `check` in CI as a drift gate. It applies every migration to a fresh
Postgres and fails the build when the result diverges from the schema the proto
declares. That way a migration that forgets a proto change (or vice versa) never
merges.

## Documentation

- [Plugin reference](cmd/protoc-gen-go-jet/README.md). The full options
  vocabulary, field-kind inference matrix, foreign keys, AIP-160 filtering,
  well-known-type mapping, nullable columns, and what the plugin refuses to
  generate.
- Options proto: [`proto/gojet/v1/options.proto`](proto/gojet/v1/options.proto).
- Worked example: [`cmd/protoc-gen-go-jet/e2e/`](cmd/protoc-gen-go-jet/e2e/), a
  self-contained suite that round-trips every supported proto kind through a
  real Postgres testcontainer.
- Agent skill: [`.claude/skills/protoc-gen-go-jet/`](.claude/skills/protoc-gen-go-jet/)
  teaches an AI coding agent the migration-authoring workflow.

## Packages

| Import | What |
|---|---|
| `cmd/protoc-gen-go-jet` | The plugin and the `check` / `diff` / `from-db` subcommands. |
| `pkg/aip` | AIP-158 keyset pagination, order-by parsing, page-token codecs. |
| `pkg/aip/jet` | `aipjet` — AIP-160 filter → go-jet `BoolExpression` translator and paginated execution. |
| `pkg/pgstore/jettypes` | Column types with custom `Scan`/`Value` (e.g. `TIMESTAMPTZ[]`). |
| `pkg/pgstore/jetgen` | Shared go-jet generator configuration. |

## Related

- [protoc-gen-go-mcp](https://github.com/redpanda-data/protoc-gen-go-mcp)
  exposes the same gRPC services as MCP servers. Same proto-first approach,
  different output.
- Request validation belongs in
  [protovalidate](https://github.com/bufbuild/protovalidate) (CEL rules on the
  proto). This plugin owns the storage contract and migration drift, not request
  validation.

## License

Apache License 2.0. See [LICENSE](LICENSE).
