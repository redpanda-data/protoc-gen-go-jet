package rpsql

import (
	"fmt"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/jackc/pgx/v5"
)

// CatalogTable makes an external Redpanda SQL catalog table (an Iceberg or
// Kafka source, e.g. default_redpanda_catalog => orders_seed_test) queryable
// through go-jet.
//
// Such tables are reachable ONLY via the non-standard `catalog => table` FROM
// operator, which go-jet cannot emit (it quotes the special characters). This
// type wraps the read in a CTE — `WITH <cte> AS (SELECT * FROM <catalog> =>
// <table>) ...` — so only that one-line seed is raw. The outer query built on
// the CTE (projections, WHERE, GROUP BY, ORDER BY, aggregates, and composite
// Field access) is fully type-safe. Usage:
//
//	orders := rpsql.NewCatalogTable("orders", "default_redpanda_catalog", "orders_seed_test")
//	status := orders.String("status")
//	stmt := postgres.WITH(orders.CTE())(
//	    postgres.SELECT(status, postgres.COUNT(postgres.STAR).AS("c")).
//	        FROM(orders.CTE()).
//	        WHERE(status.IS_NOT_NULL()).
//	        GROUP_BY(status),
//	)
//	err := rpsql.Query(ctx, querier, stmt) // or stmt.QueryContext(ctx, db, &dest)
type CatalogTable struct {
	cte postgres.CommonTableExpression
}

// NewCatalogTable builds a CatalogTable that reads `catalog => table`, exposed
// as a CTE named cteName. cteName must be a plain SQL identifier; catalog and
// table are sanitized (quoted), so names with dashes such as "orders-lk" are
// handled. The catalog is the top-level catalog name; namespace-qualified
// catalogs are not covered by this constructor.
func NewCatalogTable(cteName, catalog, table string) CatalogTable {
	if !identRe.MatchString(cteName) {
		panic(fmt.Sprintf("rpsql: invalid CTE name %q", cteName))
	}
	seed := postgres.RawStatement(fmt.Sprintf(
		"SELECT * FROM %s => %s",
		pgx.Identifier{catalog}.Sanitize(),
		pgx.Identifier{table}.Sanitize(),
	))
	return CatalogTable{cte: postgres.CTE(cteName).AS(seed)}
}

// CTE returns the underlying CTE expression. Pass it to postgres.WITH(...) and
// use it as the FROM target of the outer query.
func (c CatalogTable) CTE() postgres.CommonTableExpression { return c.cte }

// String returns a text column bound to the catalog CTE.
func (c CatalogTable) String(name string) postgres.ColumnString {
	return postgres.StringColumn(name).From(c.cte)
}

// Int returns an integer column bound to the catalog CTE.
func (c CatalogTable) Int(name string) postgres.ColumnInteger {
	return postgres.IntegerColumn(name).From(c.cte)
}

// Bool returns a boolean column bound to the catalog CTE.
func (c CatalogTable) Bool(name string) postgres.ColumnBool {
	return postgres.BoolColumn(name).From(c.cte)
}

// Float returns a float/numeric column bound to the catalog CTE.
func (c CatalogTable) Float(name string) postgres.ColumnFloat {
	return postgres.FloatColumn(name).From(c.cte)
}

// Timestampz returns a timestamptz column bound to the catalog CTE.
func (c CatalogTable) Timestampz(name string) postgres.ColumnTimestampz {
	return postgres.TimestampzColumn(name).From(c.cte)
}

// FieldString reads a text-typed member of a composite column on the catalog
// CTE, e.g. FieldString("payment", "method") builds ((orders.payment).method).
func (c CatalogTable) FieldString(column, member string) postgres.StringExpression {
	return FieldString(c.String(column), member)
}

// FieldInt reads an integer-typed member of a composite column on the CTE.
func (c CatalogTable) FieldInt(column, member string) postgres.IntegerExpression {
	return FieldInt(c.String(column), member)
}
