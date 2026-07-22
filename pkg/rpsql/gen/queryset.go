// Package gen generates go-jet table and model code for Redpanda SQL by
// introspecting a live instance through information_schema.
//
// go-jet ships a PostgreSQL generator, but its introspection queries use
// regclass and obj_description, which Redpanda SQL does not implement. QuerySet
// reimplements the two DialectQuerySet methods against information_schema only,
// then reuses go-jet's PostgreSQL code-emission templates (Redpanda SQL is wire- and
// dialect-compatible on the read side), so the generated code is identical in
// shape to any other go-jet output and imports the stock postgres builder.
package gen

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/go-jet/jet/v2/generator/metadata"
)

// QuerySet is the Redpanda SQL implementation of go-jet's metadata.DialectQuerySet.
type QuerySet struct{}

// GetTablesMetaData returns table + column metadata for a schema and table type
// (BASE TABLE or VIEW).
func (QuerySet) GetTablesMetaData(db *sql.DB, schemaName string, tableType metadata.TableType) ([]metadata.Table, error) {
	ctx := context.Background()
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_type = $2
		 ORDER BY table_name`,
		schemaName, string(tableType))
	if err != nil {
		return nil, fmt.Errorf("query %s list: %w", tableType, err)
	}

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	tables := make([]metadata.Table, 0, len(names))
	for _, name := range names {
		cols, err := columnsMetaData(ctx, db, schemaName, name)
		if err != nil {
			return nil, fmt.Errorf("columns for %s.%s: %w", schemaName, name, err)
		}
		tables = append(tables, metadata.Table{Name: name, Columns: cols})
	}
	return tables, nil
}

func columnsMetaData(ctx context.Context, db *sql.DB, schemaName, tableName string) ([]metadata.Column, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, is_nullable, data_type, udt_name
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2
		 ORDER BY ordinal_position`,
		schemaName, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []metadata.Column
	for rows.Next() {
		var name, isNullable, dataType, udtName string
		if err := rows.Scan(&name, &isNullable, &dataType, &udtName); err != nil {
			return nil, err
		}
		cols = append(cols, metadata.Column{
			Name:       name,
			IsNullable: strings.EqualFold(isNullable, "YES"),
			// Redpanda SQL has no PKs, generated columns or column defaults, so the
			// remaining flags stay false and information_schema.column_default
			// is always NULL.
			DataType: mapDataType(dataType, udtName),
		})
	}
	return cols, rows.Err()
}

// mapDataType maps an information_schema (data_type, udt_name) pair to the
// go-jet metadata.DataType that its column-type template understands.
func mapDataType(dataTypeName, udtName string) metadata.DataType {
	switch {
	case strings.EqualFold(dataTypeName, "ARRAY"):
		// Arrays report udt_name as the element type prefixed with '_'.
		return metadata.DataType{
			Name:          strings.TrimPrefix(udtName, "_"),
			Kind:          metadata.BaseType,
			Dimensions:    1,
			SourceDialect: "postgres",
		}
	case strings.EqualFold(udtName, "record") || strings.HasPrefix(dataTypeName, "("):
		// Composite/struct columns. go-jet has no composite column type; emit a
		// StringColumn (UserDefinedType maps to String) and read members with
		// rpsql.Field.
		return metadata.DataType{
			Name:          udtName,
			Kind:          metadata.UserDefinedType,
			SourceDialect: "postgres",
		}
	default:
		return metadata.DataType{
			Name:          udtName,
			Kind:          metadata.BaseType,
			SourceDialect: "postgres",
		}
	}
}

// GetEnumsMetaData returns no enums: Redpanda SQL has no native enum type (its pg_enum
// compatibility view is empty), so enum-typed proto fields are stored as text.
func (QuerySet) GetEnumsMetaData(_ *sql.DB, _ string) ([]metadata.Enum, error) {
	return nil, nil
}
