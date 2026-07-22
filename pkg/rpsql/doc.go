// Package rpsql adapts the go-jet type-safe SQL builder to Redpanda SQL.
//
// Redpanda SQL speaks the PostgreSQL wire protocol and, on the read/query side, is
// close enough to PostgreSQL that go-jet's stock `postgres` dialect can build
// the SQL directly — SELECT with joins, aggregates, window functions, CTEs,
// keyset pagination (ROW(...) > ROW(...)), array containment (= ANY(...)) and
// :: casts are all accepted. This package therefore does NOT fork go-jet or
// define a new dialect (go-jet's dialect constructor lives in an internal
// package and cannot be extended externally). Instead it layers the small set
// of RPSQL-specific adjustments on top of `github.com/go-jet/jet/v2/postgres`:
//
//   - Field / Field* : composite (struct) member access. Redpanda SQL accesses a
//     composite column's members as (col).member, NOT col.member (which parses
//     as table.column). go-jet has no builder for this; Field supplies it.
//   - QueryExecMode   : Redpanda SQL describes untyped bind parameters as text (OID 25),
//     so pgx's default statement-cache mode fails to encode non-text params.
//     Queries must run under pgx.QueryExecModeExec. See ExecModeArgs / the
//     package-level guidance below.
//
// The gen subpackage generates go-jet table/model code by introspecting a live
// Redpanda SQL instance through information_schema (go-jet's own PostgreSQL generator
// relies on regclass/obj_description, which Redpanda SQL does not implement).
//
// # Redpanda SQL read-side constraints
//
// The following PostgreSQL constructs are NOT supported by Redpanda SQL and must be
// avoided when building queries:
//
//   - ILIKE (use LOWER(col) LIKE ... instead)
//   - DISTINCT ON (...)
//   - RETURNING and INSERT ... ON CONFLICT (this package targets reads)
//   - JSON path operators #> and #>> (parse error). The key/index arrow
//     operators -> and ->> DO work on json/jsonb columns (including chaining
//     and integer indexing, e.g. jb -> 'a' ->> 'b', jb -> 'arr' -> 0), but
//     go-jet has no builder for them, so use postgres.RawString / a custom
//     expression. Arrows do NOT work on composite columns — use Field there.
//
// Composite columns are generated as StringColumn (go-jet has no composite
// column type); use Field to read their members in a type-safe way.
//
// # External (catalog) tables
//
// Iceberg/Kafka sources in a Redpanda SQL catalog are read with the
// non-standard FROM operator `catalog => table`
// (e.g. FROM default_redpanda_catalog => orders_seed_test). They are NOT
// reachable through a plain schema.table reference, and go-jet's typed table
// API cannot emit the => operator (it quotes the special characters).
//
// This is the common case, so CatalogTable wraps the read in a CTE — only the
// one-line seed is raw, and the outer query (projections, WHERE, GROUP BY,
// ORDER BY, aggregates and composite Field access) is fully type-safe:
//
//	orders := rpsql.NewCatalogTable("orders", "default_redpanda_catalog", "orders_seed_test")
//	status := orders.String("status")
//	stmt := postgres.WITH(orders.CTE())(
//	    postgres.SELECT(status, orders.FieldString("payment", "method"), postgres.COUNT(postgres.STAR)).
//	        FROM(orders.CTE()).WHERE(status.IS_NOT_NULL()).GROUP_BY(status),
//	)
//	err := rpsql.Query(ctx, querier, stmt)
//
// For a one-off read where typing buys little, postgres.RawStatement with a raw
// => FROM also works (named params + struct scanning are preserved).
//
// The typed builder and generated tables also apply to managed tables in a
// regular schema (public.*).
package rpsql
