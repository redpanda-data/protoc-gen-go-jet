package gen

import (
	"database/sql"
	"fmt"

	"github.com/go-jet/jet/v2/generator/metadata"
	"github.com/go-jet/jet/v2/generator/template"
	"github.com/go-jet/jet/v2/postgres"
)

// GenerateDB introspects schemaName on the Redpanda SQL instance behind db and writes
// go-jet table + model packages under destDir. db must be opened with the pgx
// stdlib driver configured for bearer auth and QueryExecModeExec (see the
// rpsql-jet-gen command).
func GenerateDB(db *sql.DB, schemaName, destDir string) error {
	schemaMeta, err := metadata.GetSchema(db, QuerySet{}, schemaName)
	if err != nil {
		return fmt.Errorf("introspect schema %q: %w", schemaName, err)
	}
	// Redpanda SQL is dialect-compatible with PostgreSQL on the read side, so the stock
	// postgres templates emit correct, compiling go-jet code (imports
	// github.com/go-jet/jet/v2/postgres).
	if err := template.ProcessSchema(destDir, schemaMeta, template.Default(postgres.Dialect)); err != nil {
		return fmt.Errorf("generate schema %q: %w", schemaName, err)
	}
	return nil
}
