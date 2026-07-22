// Command rpsql-jet-gen generates go-jet table/model code by introspecting a
// live Redpanda SQL instance.
//
// It connects over the PostgreSQL wire protocol, using bearer (OIDC) auth when
// a token is supplied and basic auth otherwise, and always sets
// QueryExecModeExec (required by Redpanda SQL's text-defaulted bind parameters).
//
// Example (localdev, OIDC bearer):
//
//	TOKEN=$(bash tools/localdev/scripts/test-rpsql.sh --print-token)
//	rpsql-jet-gen \
//	  -host 127.0.0.1 -port 30432 -db redpanda \
//	  -token "$TOKEN" -schema public -out ./gen/rpsql
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/rpsql/gen"
)

func main() {
	var (
		host     = flag.String("host", "127.0.0.1", "Redpanda SQL host")
		port     = flag.Int("port", 5432, "Redpanda SQL port")
		dbName   = flag.String("db", "redpanda", "database name")
		schema   = flag.String("schema", "public", "schema to introspect")
		out      = flag.String("out", "./rpsql", "output directory")
		token    = flag.String("token", "", "OIDC bearer token; enables bearer auth (username is ignored)")
		user     = flag.String("user", "", "username for basic auth (ignored when -token is set)")
		password = flag.String("password", "", "password for basic auth")
		sslMode  = flag.String("sslmode", "disable", "sslmode (disable, require, ...)")
	)
	flag.Parse()

	db, err := open(*host, *port, *dbName, *schema, *token, *user, *password, *sslMode)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()

	if err := gen.GenerateDB(db, *schema, *out); err != nil {
		log.Fatalf("generate: %v", err)
	}
	fmt.Printf("generated go-jet code for schema %q into %s\n", *schema, *out)
}

func open(host string, port int, dbName, _ /*schema*/, token, user, password, sslMode string) (*sql.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s sslmode=%s", host, port, dbName, sslMode)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	if token != "" {
		// Bearer (OIDC) auth: username is ignored by Redpanda SQL, the JWT is the
		// password and auth_method=bearer is passed as a startup option.
		cfg.User = "ignored"
		cfg.Password = token
		opts := cfg.RuntimeParams["options"]
		cfg.RuntimeParams["options"] = strings.TrimSpace(opts + " -c auth_method=bearer")
	} else {
		if user == "" {
			return nil, fmt.Errorf("either -token (bearer) or -user/-password (basic) is required")
		}
		cfg.User = user
		cfg.Password = password
	}
	// Required for Redpanda SQL: it describes untyped bind parameters as text, so the
	// default statement-cache mode cannot encode non-text params.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec

	db, err := sql.Open("pgx", stdlib.RegisterConnConfig(cfg))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
