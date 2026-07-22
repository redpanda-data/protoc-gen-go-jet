package rpsql

import (
	"context"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/jackc/pgx/v5"
)

// Querier runs a query and is satisfied by *pgxpool.Pool, *pgxpool.Conn and
// *pgx.Conn. It mirrors the Querier in package sql so a go-jet statement can be
// executed against the enterprise per-user Redpanda SQL pools.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ExecArgs prepends pgx.QueryExecModeExec to a go-jet statement's bind
// arguments so the query runs under the simple/exec protocol.
//
// This is REQUIRED for Redpanda SQL: Redpanda SQL describes untyped bind parameters as text
// (OID 25), so pgx's default statement-cache mode cannot encode non-text
// parameters (e.g. an integer bound to `col > $1`). QueryExecModeExec sidesteps
// the Describe round-trip. Usage:
//
//	stmt := postgres.SELECT(...)...
//	sqlStr, args := stmt.Sql()
//	rows, err := querier.Query(ctx, sqlStr, rpsql.ExecArgs(args)...)
func ExecArgs(args []any) []any {
	out := make([]any, 0, len(args)+1)
	out = append(out, pgx.QueryExecModeExec)
	out = append(out, args...)
	return out
}

// Query executes a go-jet statement against a pgx Querier using the RPSQL-safe
// exec mode and returns the raw pgx.Rows. The caller owns scanning and closing
// the rows. It exists so callers reuse the enterprise Redpanda SQL connection pools
// (package sql) rather than opening a separate database/sql handle.
func Query(ctx context.Context, q Querier, stmt postgres.Statement) (pgx.Rows, error) {
	sqlStr, args := stmt.Sql()
	return q.Query(ctx, sqlStr, ExecArgs(args)...)
}
