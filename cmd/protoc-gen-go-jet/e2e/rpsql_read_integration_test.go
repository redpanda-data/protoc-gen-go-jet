//go:build integration

// Executes the generated (gojet.v1.rpsql_read) read model against a live
// Redpanda SQL instance. Skips unless a connection is provided:
//
//	RPSQL_TEST_HOST  (default 127.0.0.1)
//	RPSQL_TEST_PORT  (default 5432)
//	RPSQL_TEST_DB    (default redpanda)
//	RPSQL_TEST_TOKEN bearer/OIDC JWT (enables bearer auth)
//	RPSQL_TEST_USER / RPSQL_TEST_PASSWORD  basic auth (used when no token)
//
// The RpsqlOrder fixture points at default_redpanda_catalog => orders_seed_test,
// a neutral orders fixture seeded in localdev.
package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	e2ev1 "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1"
	storage "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1/storage"
)

// TestRpsqlOrderRead_SelectProto exercises the generated Select(), which reads
// the catalog table and returns fully-assembled proto messages (no bespoke
// struct): scalars, enum (text->enum), timestamp, and nested composite
// messages. Repeated fields stay zero-valued (rpsql array limitation).
func TestRpsqlOrderRead_SelectProto(t *testing.T) {
	db := openRpsql(t)
	defer db.Close()

	orders, err := storage.NewRpsqlOrderRead().Select(context.Background(), db)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(orders) == 0 {
		t.Skip("no rows in orders_seed_test")
	}

	var got *e2ev1.RpsqlOrder
	for _, o := range orders {
		if o.GetCustomer().GetShippingAddress() != nil && o.GetPayment() != nil {
			got = o
			break
		}
	}
	if got == nil {
		t.Fatal("expected an order with customer.shipping_address and payment populated")
	}
	if got.GetStatus() == e2ev1.OrderStatus_ORDER_STATUS_UNSPECIFIED && got.GetOrderId() == "" {
		t.Errorf("proto not populated: %+v", got)
	}
	if len(got.GetItems()) != 0 {
		t.Errorf("repeated Items should be empty (rpsql cannot read arrays), got %d", len(got.GetItems()))
	}
	t.Logf("*RpsqlOrder: status=%s method=%s country=%s total=%.2f items=%d",
		got.GetStatus(),
		got.GetPayment().GetMethod(),
		got.GetCustomer().GetShippingAddress().GetCountry(),
		got.GetTotal(),
		len(got.GetItems()),
	)
}

func TestRpsqlOrderRead_Integration(t *testing.T) {
	db := openRpsql(t)
	defer db.Close()

	o := storage.NewRpsqlOrderRead()
	stmt := postgres.WITH(o.CTE())(
		postgres.SELECT(
			o.Status().AS("row.status"),
			o.PaymentMethod().AS("row.method"),
			o.CustomerShippingAddressCountry().AS("row.country"),
			postgres.SUM(o.Total()).AS("row.revenue"),
			postgres.COUNT(postgres.STAR).AS("row.orders"),
		).FROM(o.CTE()).
			WHERE(o.Status().IS_NOT_NULL()).
			GROUP_BY(o.Status(), o.PaymentMethod(), o.CustomerShippingAddressCountry()).
			ORDER_BY(postgres.COUNT(postgres.STAR).DESC()).
			LIMIT(5),
	)

	var dest []struct {
		Status  string  `alias:"row.status"`
		Method  string  `alias:"row.method"`
		Country string  `alias:"row.country"`
		Revenue float64 `alias:"row.revenue"`
		Orders  int64   `alias:"row.orders"`
	}
	if err := stmt.QueryContext(context.Background(), db, &dest); err != nil {
		q, _ := stmt.Sql()
		t.Fatalf("query: %v\nSQL:\n%s", err, q)
	}
	if len(dest) == 0 {
		t.Skip("no rows in orders_seed_test; seed localdev to assert results")
	}
	for i := 1; i < len(dest); i++ {
		if dest[i-1].Orders < dest[i].Orders {
			t.Errorf("ORDER BY COUNT DESC not honoured: %+v", dest)
		}
	}
	t.Logf("generated read model over orders_seed_test: %+v", dest)
}

func openRpsql(t *testing.T) *sql.DB {
	t.Helper()
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s sslmode=disable",
		env("RPSQL_TEST_HOST", "127.0.0.1"), env("RPSQL_TEST_PORT", "5432"), env("RPSQL_TEST_DB", "redpanda")))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if tok := os.Getenv("RPSQL_TEST_TOKEN"); tok != "" {
		cfg.User = "ignored"
		cfg.Password = tok
		cfg.RuntimeParams["options"] = "-c auth_method=bearer"
	} else if u := os.Getenv("RPSQL_TEST_USER"); u != "" {
		cfg.User = u
		cfg.Password = os.Getenv("RPSQL_TEST_PASSWORD")
	} else {
		t.Skip("set RPSQL_TEST_TOKEN or RPSQL_TEST_USER to run the rpsql read-model integration test")
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec

	db, err := sql.Open("pgx", stdlib.RegisterConnConfig(cfg))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("rpsql unreachable (%v)", err)
	}
	return db
}
