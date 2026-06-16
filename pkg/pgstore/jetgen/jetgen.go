// Package jetgen wraps go-jet's Postgres template generator with this
// project's house conventions, so every emitted model struct uses the
// same type mapping (string for JSONB, not []byte; pq.StringArray for
// text[]; schema_migrations always skipped).
//
// Custom Scanner/Valuer types for specific (table, column) pairs are
// supplied via Config.ColumnOverrides; a JSONB-backed map type with
// custom Scan/Value is the canonical example. When no override is
// registered the column keeps the go-jet default.
package jetgen

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/go-jet/jet/v2/generator/metadata"
	pggen "github.com/go-jet/jet/v2/generator/postgres"
	"github.com/go-jet/jet/v2/generator/template"
	pgdialect "github.com/go-jet/jet/v2/postgres"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver
)

// DefaultSchema is the Postgres schema name all PG-backed resource
// tables live under. Callers rarely need to override.
const DefaultSchema = "public"

// Config controls per-run jet generation behaviour. All fields are
// optional; a zero Config is legal and produces jet's stock output
// for every non-migration table.
type Config struct {
	// Schema is the Postgres schema to introspect. Defaults to
	// "public" when empty.
	Schema string

	// ColumnOverrides maps table name → column name → Go type for
	// columns that need a custom Scanner/Valuer. Columns not listed
	// here keep go-jet's default mapping (string for JSONB, bool for
	// BOOLEAN, etc.).
	ColumnOverrides map[string]map[string]template.Type

	// SkipTables lists tables the introspector should ignore. Always
	// includes schema_migrations (appended internally).
	SkipTables map[string]struct{}
}

// GenerateFromDB introspects the DSN-connected database and writes
// go-jet model + sql-builder packages under outDir. Intended for
// callers that already have a database with the schema applied —
// tools invoked against a dev DB, test harnesses, the protoc plugin
// after it has applied ddl.sql files.
func GenerateFromDB(ctx context.Context, dsn, outDir string, cfg Config) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return GenerateFromSQLDB(db, outDir, cfg)
}

// GenerateFromSQLDB is the variant callers use when they already hold
// a live *sql.DB (e.g. the plugin's ephemeral testcontainer).
func GenerateFromSQLDB(db *sql.DB, outDir string, cfg Config) error {
	schema := cfg.Schema
	if schema == "" {
		schema = DefaultSchema
	}

	// Always skip migration bookkeeping tables even when the caller
	// omits SkipTables — no codegen consumer wants a typed
	// schema_migrations helper.
	skip := map[string]struct{}{"schema_migrations": {}}
	for k := range cfg.SkipTables {
		skip[k] = struct{}{}
	}

	tmpl := template.Default(pgdialect.Dialect).UseSchema(customiseSchema(cfg.ColumnOverrides, skip))
	return pggen.GenerateDB(db, schema, outDir, tmpl)
}

// customiseSchema builds the jet template hook that drops skipped
// tables and threads columnOverrides through to each field. Factored
// out so the runtime config flows as closures rather than package-
// level state.
func customiseSchema(overrides map[string]map[string]template.Type, skip map[string]struct{}) func(metadata.Schema) template.Schema {
	return func(s metadata.Schema) template.Schema {
		return template.DefaultSchema(s).
			UseModel(template.DefaultModel().UseTable(func(t metadata.Table) template.TableModel {
				if _, ok := skip[t.Name]; ok {
					return template.TableModel{Skip: true}
				}
				return template.DefaultTableModel(t).UseField(func(c metadata.Column) template.TableModelField {
					field := template.DefaultTableModelField(c)
					if cols, ok := overrides[t.Name]; ok {
						if typ, ok := cols[c.Name]; ok {
							// For nullable columns, jet's built-in
							// types map to pointers automatically
							// (getType → IsNullable prepends `*`).
							// Overrides bypass that path — the
							// supplied Type is literal — so we apply
							// the same promotion here. The mapper
							// generator on the plugin side assumes
							// `*<custom>` for nullable collection
							// kinds (e.g. `*jettypes.TimestampArray`).
							if c.IsNullable && !strings.HasPrefix(typ.Name, "*") {
								typ.Name = "*" + typ.Name
							}
							field.Type = typ
						}
					}
					return field
				})
			})).
			UseSQLBuilder(template.DefaultSQLBuilder().UseTable(func(t metadata.Table) template.TableSQLBuilder {
				if _, ok := skip[t.Name]; ok {
					return template.TableSQLBuilder{Skip: true}
				}
				return template.DefaultTableSQLBuilder(t)
			}))
	}
}
