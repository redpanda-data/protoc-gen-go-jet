# rpsql

Type-safe SQL for **Redpanda SQL** built on the
[go-jet](https://github.com/go-jet/jet) query builder.

Redpanda SQL speaks the PostgreSQL wire protocol and is dialect-compatible with
PostgreSQL on the **read/query** side, so this package layers over go-jet's
stock `postgres` builder rather than forking it (go-jet's dialect constructor is
internal and cannot be extended externally). It supplies only the small set of
RPSQL-specific adjustments needed to query Redpanda SQL correctly.

## What it provides

| Piece | Purpose |
|---|---|
| `Field`, `FieldString`, `FieldInt`, … | Composite/struct member access. Redpanda SQL reads a composite column's members as `(col).member`; go-jet has no builder for this. |
| `ExecArgs`, `Query` | Run a go-jet statement against a pgx `Querier` under `QueryExecModeExec` (required — Redpanda SQL types untyped bind params as `text`). |
| `gen` subpackage + `rpsql-jet-gen` | Generate go-jet table/model code by introspecting a live Redpanda SQL via `information_schema` (go-jet's own generator relies on `regclass`, which Redpanda SQL lacks). |

## Query example

```go
import (
	"github.com/go-jet/jet/v2/postgres"
	"github.com/redpanda-data/backend/console-enterprise/pkg/rpsql"
)

// composite member access: WHERE (payment).method = $1
stmt := postgres.SELECT(
	tbl.Region,
	postgres.SUM(tbl.Total).AS("revenue"),
).FROM(tbl).
	WHERE(rpsql.FieldString(tbl.Payment, "method").EQ(postgres.String("card"))).
	GROUP_BY(tbl.Region)

// Against the enterprise per-user Redpanda SQL pool (package sql Querier):
rows, err := rpsql.Query(ctx, querier, stmt) // uses QueryExecModeExec

// Standalone (database/sql opened with DefaultQueryExecMode = QueryExecModeExec):
err = stmt.QueryContext(ctx, db, &dest)
```

## Generating table types

```bash
# localdev, OIDC bearer auth
TOKEN=$(bash tools/localdev/scripts/test-rpsql.sh --print-token)
go run ./pkg/rpsql/gen/cmd/rpsql-jet-gen \
  -host 127.0.0.1 -port 30432 -db redpanda \
  -token "$TOKEN" -schema public -out ./gen/rpsql
```

Composite columns are generated as `StringColumn` (go-jet has no composite
column type); read their members with `Field`.

## Redpanda SQL read-side constraints

Avoid these unsupported PostgreSQL constructs when building queries:

- `ILIKE` → use `LOWER(col) LIKE …`
- `DISTINCT ON (…)`
- `RETURNING`, `INSERT … ON CONFLICT` (this package targets reads)
- JSON path operators `#>` / `#>>` → **parse error**. The arrow operators
  `->` / `->>` *do* work on `json`/`jsonb` (chaining and integer indexing
  included), but go-jet has no builder for them — use `postgres.RawString` or a
  custom expression. Arrows don't work on composite columns (use `Field`).

## External (catalog) tables — the primary use case

Catalog-backed Iceberg/Kafka sources are read with the non-standard
`catalog => table` operator and are **not** reachable via a plain `schema.table`
reference. go-jet's typed table API cannot emit `=>`.

`CatalogTable` wraps the read in a CTE so **only the one-line seed is raw** and
the whole outer query stays type-safe, including composite (and nested
composite) `Field` access:

```go
orders := rpsql.NewCatalogTable("orders", "default_redpanda_catalog", "orders_seed_test")
status := orders.String("status")

stmt := postgres.WITH(orders.CTE())(
	postgres.SELECT(
		status.AS("row.status"),
		orders.FieldString("payment", "method").AS("row.method"), // ((orders.payment).method)
		postgres.SUM(orders.Float("total")).AS("row.revenue"),
		postgres.COUNT(postgres.STAR).AS("row.c"),
	).FROM(orders.CTE()).
		WHERE(status.IS_NOT_NULL()).
		GROUP_BY(status, orders.FieldString("payment", "method")).
		ORDER_BY(postgres.COUNT(postgres.STAR).DESC()).
		LIMIT(5),
)
// -> WITH orders AS (SELECT * FROM "default_redpanda_catalog" => "orders_seed_test") SELECT ...
err := rpsql.Query(ctx, querier, stmt) // verified vs a localdev orders fixture
```

Identifiers are sanitized, so dashed names like `orders-lk` work. For a one-off
read where typing buys little, `postgres.RawStatement` with a raw `=>` FROM also
works (params + struct scanning preserved).

The typed builder and generated tables also apply to managed tables in a regular
schema (`public.*`).

## Tests

```bash
go test ./pkg/rpsql/...                       # unit (SQL serialization)

# integration against a live Redpanda SQL:
RPSQL_TEST_PORT=30432 RPSQL_TEST_TOKEN="$TOKEN" \
  go test -tags integration ./pkg/rpsql/ -run Integration -v
```
