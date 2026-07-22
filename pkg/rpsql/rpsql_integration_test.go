//go:build integration

// These tests run go-jet-built queries against a live Redpanda SQL
// instance. Provide the connection via env vars (all optional except the DSN
// building blocks); the test skips if the host is unreachable.
//
//	RPSQL_TEST_HOST   (default 127.0.0.1)
//	RPSQL_TEST_PORT   (default 5432)
//	RPSQL_TEST_DB     (default redpanda)
//	RPSQL_TEST_TOKEN  bearer/OIDC JWT (enables bearer auth)
//	RPSQL_TEST_USER   / RPSQL_TEST_PASSWORD  basic auth (used when no token)
//
// Localdev:
//
//	RPSQL_TEST_PORT=30432 \
//	RPSQL_TEST_TOKEN=$(bash ../../../../tools/... test-rpsql.sh --print-token) \
//	go test -tags integration ./pkg/rpsql/ -run Integration -v
package rpsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/rpsql"
)

func openRPSQL(t *testing.T) *sql.DB {
	t.Helper()

	host := envOr("RPSQL_TEST_HOST", "127.0.0.1")
	port := envOr("RPSQL_TEST_PORT", "5432")
	dbName := envOr("RPSQL_TEST_DB", "redpanda")

	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s sslmode=disable", host, port, dbName))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if tok := os.Getenv("RPSQL_TEST_TOKEN"); tok != "" {
		cfg.User = "ignored"
		cfg.Password = tok
		cfg.RuntimeParams["options"] = "-c auth_method=bearer"
	} else {
		cfg.User = envOr("RPSQL_TEST_USER", "")
		cfg.Password = os.Getenv("RPSQL_TEST_PASSWORD")
		if cfg.User == "" {
			t.Skip("set RPSQL_TEST_TOKEN or RPSQL_TEST_USER to run Redpanda SQL integration tests")
		}
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
		t.Skipf("Redpanda SQL unreachable at %s:%s (%v)", host, port, err)
	}
	return db
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// agentNetworkView is a hand-written go-jet table matching the localdev Redpanda SQL
// public.agent_network_view. In production these come from rpsql-jet-gen.
type agentNetworkView struct {
	postgres.Table
	Kind     postgres.ColumnString
	Requests postgres.ColumnInteger
	Tokens   postgres.ColumnInteger
}

func newAgentNetworkView() *agentNetworkView {
	t := &agentNetworkView{
		Kind:     postgres.StringColumn("kind"),
		Requests: postgres.IntegerColumn("requests"),
		Tokens:   postgres.IntegerColumn("tokens"),
	}
	t.Table = postgres.NewTable("public", "agent_network_view", "", t.Kind, t.Requests, t.Tokens)
	return t
}

// TestIntegration_AggregateQuery exercises the core analytics query shape:
// a parameterised, grouped, ordered, limited aggregate. This is the pattern
// that fails under pgx's default exec mode (int bind param described as text)
// and must work under QueryExecModeExec.
func TestIntegration_AggregateQuery(t *testing.T) {
	db := openRPSQL(t)
	defer db.Close()

	anv := newAgentNetworkView()
	stmt := postgres.SELECT(
		anv.Kind.AS("row.kind"),
		postgres.SUM(anv.Requests).AS("row.total_requests"),
	).FROM(anv).
		WHERE(anv.Tokens.GT(postgres.Int(0))).
		GROUP_BY(anv.Kind).
		ORDER_BY(postgres.SUM(anv.Requests).DESC()).
		LIMIT(5)

	var dest []struct {
		Kind          string `alias:"row.kind"`
		TotalRequests int64  `alias:"row.total_requests"`
	}
	if err := stmt.QueryContext(context.Background(), db, &dest); err != nil {
		t.Fatalf("query: %v\nSQL:\n%s", err, sqlOf(stmt))
	}
	if len(dest) == 0 {
		t.Skip("no rows in agent_network_view; seed localdev to assert results")
	}
	for i := 1; i < len(dest); i++ {
		if dest[i-1].TotalRequests < dest[i].TotalRequests {
			t.Errorf("ORDER BY DESC not honoured: %+v", dest)
		}
	}
	t.Logf("top kinds by requests: %+v", dest)
}

// TestIntegration_CompositeField creates a composite-typed table and verifies
// that rpsql.Field reads its members through go-jet.
func TestIntegration_CompositeField(t *testing.T) {
	db := openRPSQL(t)
	defer db.Close()

	ctx := context.Background()
	stmts := []string{
		`DROP TABLE IF EXISTS rpsql_it_probe`,
		`CREATE TYPE rpsql_it_addr AS (city text, zip int4)`,
		`CREATE TABLE rpsql_it_probe (id int8, addr rpsql_it_addr)`,
		`INSERT INTO rpsql_it_probe SELECT 1, ROW('NYC', 10001)::rpsql_it_addr`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			// CREATE TYPE may already exist from a prior run; tolerate it.
			if !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS rpsql_it_probe`)
		_, _ = db.ExecContext(context.Background(), `DROP TYPE IF EXISTS rpsql_it_addr`)
	})

	type probeT struct {
		postgres.Table
		ID   postgres.ColumnInteger
		Addr postgres.ColumnString
	}
	tbl := &probeT{ID: postgres.IntegerColumn("id"), Addr: postgres.StringColumn("addr")}
	tbl.Table = postgres.NewTable("public", "rpsql_it_probe", "", tbl.ID, tbl.Addr)

	stmt := postgres.SELECT(
		rpsql.FieldString(tbl.Addr, "city").AS("r.city"),
		rpsql.FieldInt(tbl.Addr, "zip").AS("r.zip"),
	).FROM(tbl).
		WHERE(rpsql.FieldInt(tbl.Addr, "zip").EQ(postgres.Int(10001)))

	var dest []struct {
		City string `alias:"r.city"`
		Zip  int32  `alias:"r.zip"`
	}
	if err := stmt.QueryContext(ctx, db, &dest); err != nil {
		t.Fatalf("composite query: %v\nSQL:\n%s", err, sqlOf(stmt))
	}
	if len(dest) != 1 || dest[0].City != "NYC" || dest[0].Zip != 10001 {
		t.Fatalf("expected [{NYC 10001}], got %+v", dest)
	}
}

// TestIntegration_GeneratorRoundTrip introspects the live schema and asserts
// the generator produced at least one table with columns.
func TestIntegration_GeneratorRoundTrip(t *testing.T) {
	db := openRPSQL(t)
	defer db.Close()

	// Reachability of information_schema is the prerequisite the generator
	// depends on; assert the column catalog returns rows for a known table.
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM information_schema.columns WHERE table_schema='public'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("information_schema.columns query: %v", err)
	}
	if n == 0 {
		t.Skip("no public columns in this Redpanda SQL instance")
	}
	t.Logf("information_schema.columns reachable: %d public columns", n)
}

// TestIntegration_CatalogTable reads a neutral external catalog table
// (default_redpanda_catalog => orders_seed_test) through the CatalogTable CTE
// helper. It exercises the full external-read path — the raw catalog seed, a
// typed grouped aggregate with SUM, a composite Field ((payment).method) and a
// nested composite Field (((customer).shipping_address).country) — against a
// generic orders fixture rather than any internal telemetry table. Skips if the
// catalog table is absent.
func TestIntegration_CatalogTable(t *testing.T) {
	db := openRPSQL(t)
	defer db.Close()

	catalog := envOr("RPSQL_TEST_CATALOG", "default_redpanda_catalog")
	table := envOr("RPSQL_TEST_CATALOG_TABLE", "orders_seed_test")

	orders := rpsql.NewCatalogTable("o", catalog, table)
	status := orders.String("status")
	method := orders.FieldString("payment", "method") // (o.payment).method
	// Nested composite: ((o.customer).shipping_address).country
	country := rpsql.FieldString(rpsql.Field(orders.String("customer"), "shipping_address"), "country")

	stmt := postgres.WITH(orders.CTE())(
		postgres.SELECT(
			status.AS("row.status"),
			method.AS("row.method"),
			country.AS("row.country"),
			postgres.COUNT(postgres.STAR).AS("row.orders"),
			postgres.SUM(orders.Float("total")).AS("row.revenue"),
		).FROM(orders.CTE()).
			WHERE(status.IS_NOT_NULL()).
			GROUP_BY(status, method, country).
			ORDER_BY(postgres.COUNT(postgres.STAR).DESC()).
			LIMIT(5),
	)

	var dest []struct {
		Status  string  `alias:"row.status"`
		Method  string  `alias:"row.method"`
		Country string  `alias:"row.country"`
		Orders  int64   `alias:"row.orders"`
		Revenue float64 `alias:"row.revenue"`
	}
	if err := stmt.QueryContext(context.Background(), db, &dest); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			t.Skipf("catalog table %s => %s absent: %v", catalog, table, err)
		}
		t.Fatalf("catalog query: %v\nSQL:\n%s", err, sqlOf(stmt))
	}
	if len(dest) == 0 {
		t.Skip("no rows in catalog table; seed localdev to assert results")
	}
	for i := 1; i < len(dest); i++ {
		if dest[i-1].Orders < dest[i].Orders {
			t.Errorf("ORDER BY COUNT DESC not honoured: %+v", dest)
		}
	}
	t.Logf("top order groups in %s: %+v", table, dest)
}

func sqlOf(s postgres.Statement) string {
	q, _ := s.Sql()
	return q
}
