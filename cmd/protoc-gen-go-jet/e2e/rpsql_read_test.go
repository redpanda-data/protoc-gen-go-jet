package e2e

import (
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/postgres"

	storage "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1/storage"
)

// TestRpsqlOrderRead_GeneratedSQL exercises the generated (gojet.v1.rpsql_read)
// read model end-to-end at the SQL-building level: the catalog CTE seed, a
// scalar/enum column, a single composite member, a nested composite member, and
// an aggregate — all typed, no database required.
func TestRpsqlOrderRead_GeneratedSQL(t *testing.T) {
	o := storage.NewRpsqlOrderRead()

	stmt := postgres.WITH(o.CTE())(
		postgres.SELECT(
			o.Status().AS("row.status"),
			o.PaymentMethod().AS("row.method"),
			o.CustomerShippingAddressCountry().AS("row.country"),
			postgres.SUM(o.Total()).AS("row.revenue"),
			postgres.COUNT(postgres.STAR).AS("row.n"),
		).FROM(o.CTE()).
			WHERE(o.Status().IS_NOT_NULL()).
			GROUP_BY(o.Status(), o.PaymentMethod(), o.CustomerShippingAddressCountry()).
			ORDER_BY(postgres.COUNT(postgres.STAR).DESC()).
			LIMIT(5),
	)

	query, _ := stmt.Sql()
	for _, want := range []string{
		`WITH o AS (SELECT * FROM "default_redpanda_catalog" => "orders_seed_test"`, // raw catalog seed
		"(o.payment).method",            // single composite member
		"(o.customer).shipping_address", // nested composite (outer level)
		").country",                     // nested composite (leaf member)
		"o.status",                      // enum -> text column
	} {
		if !strings.Contains(query, want) {
			t.Errorf("generated query missing %q:\n%s", want, query)
		}
	}
}
