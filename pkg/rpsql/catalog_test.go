package rpsql_test

import (
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/rpsql"
)

func TestCatalogTable_SQL(t *testing.T) {
	orders := rpsql.NewCatalogTable("o", "default_redpanda_catalog", "orders_seed_test")
	status := orders.String("status")

	stmt := postgres.WITH(orders.CTE())(
		postgres.SELECT(
			status.AS("row.status"),
			orders.FieldString("payment", "method").AS("row.method"),
			postgres.COUNT(postgres.STAR).AS("row.c"),
		).FROM(orders.CTE()).
			WHERE(status.IS_NOT_NULL()).
			GROUP_BY(status, orders.FieldString("payment", "method")).
			ORDER_BY(postgres.COUNT(postgres.STAR).DESC()).
			LIMIT(5),
	)

	query, args := stmt.Sql()

	// Seed is raw and uses the => operator with sanitized identifiers; go-jet
	// must NOT column-list the CTE (Redpanda SQL rejects `WITH o (..) AS`).
	if !strings.Contains(query, `WITH o AS (SELECT * FROM "default_redpanda_catalog" => "orders_seed_test"`) {
		t.Errorf("unexpected CTE seed:\n%s", query)
	}
	if strings.Contains(query, "WITH o (") {
		t.Errorf("CTE column list is unsupported by Redpanda SQL but was emitted:\n%s", query)
	}
	// Composite access on the CTE column must be parenthesised.
	if !strings.Contains(query, "(o.payment).method") {
		t.Errorf("expected composite access (o.payment).method:\n%s", query)
	}
	if len(args) != 1 || args[0] != int64(5) {
		t.Errorf("expected LIMIT bind arg 5, got %v", args)
	}
}

func TestNewCatalogTable_SanitizesDashedName(t *testing.T) {
	ct := rpsql.NewCatalogTable("t", "default_redpanda_catalog", "orders-lk")
	stmt := postgres.SELECT(postgres.COUNT(postgres.STAR)).FROM(ct.CTE())
	query, _ := postgres.WITH(ct.CTE())(stmt).Sql()
	if !strings.Contains(query, `=> "orders-lk"`) {
		t.Errorf("dashed table name must be quoted in the => seed:\n%s", query)
	}
}

func TestNewCatalogTable_RejectsBadCTEName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for invalid CTE name")
		}
	}()
	_ = rpsql.NewCatalogTable("bad name", "c", "t")
}
