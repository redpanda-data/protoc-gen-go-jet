package jet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

// Execute runs a complete paginated list query using go-jet.
func Execute[Model any](
	ctx context.Context,
	schema *Schema[Model],
	params aip.Params,
	baseQuery postgres.SelectStatement,
	db qrm.Queryable,
) ([]Model, string, error) {
	return ExecuteWithCondition(ctx, schema, params, baseQuery, nil, db)
}

// ExecuteWithCondition runs a paginated list query while preserving a fixed
// base WHERE condition (e.g. environment_id = ?). The base condition and the
// keyset cursor condition are combined with AND.
func ExecuteWithCondition[Model any](
	ctx context.Context,
	schema *Schema[Model],
	params aip.Params,
	baseQuery postgres.SelectStatement,
	baseCondition postgres.BoolExpression,
	db qrm.Queryable,
) ([]Model, string, error) {
	plan, err := BuildPlan(schema, params)
	if err != nil {
		return nil, "", err
	}

	cursorCond, err := buildKeysetCondition(plan.OrderBy, plan.CursorValues, schema.fields)
	if err != nil {
		return nil, "", wrapAIPError(err, aip.ErrInvalidPageToken)
	}

	stmt := baseQuery.
		ORDER_BY(orderByClauses(schema.fields, plan.OrderBy)...).
		LIMIT(int64(plan.PageSize + 1))

	if where := combineConditions(baseCondition, cursorCond); where != nil {
		stmt = stmt.WHERE(where)
	}

	var rows []Model
	if err := stmt.QueryContext(ctx, db, &rows); err != nil {
		if errors.Is(err, qrm.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("query execution failed: %w", err)
	}

	nextToken, err := buildNextPageToken(schema, plan, rows)
	if err != nil {
		return nil, "", err
	}

	if len(rows) > int(plan.PageSize) {
		rows = rows[:plan.PageSize]
	}

	return rows, nextToken, nil
}

// buildNextPageToken creates the opaque next_page_token for the response.
func buildNextPageToken[Model any](schema *Schema[Model], plan *aip.Plan, rows []Model) (string, error) {
	size := int(plan.PageSize)
	if len(rows) <= size {
		return "", nil
	}

	cursorVals, err := schema.extractCursorValues(&rows[size-1], plan.OrderBy)
	if err != nil {
		return "", fmt.Errorf("failed to extract cursor values: %w", err)
	}

	return aip.EncodeToken(schema.resourceType, cursorVals, plan.OrderBy, plan.Filter, schema.codecs())
}

// orderByClauses converts an OrderBy into go-jet ORDER BY clauses.
func orderByClauses[M any](fields Fields[M], order aip.OrderBy) []postgres.OrderByClause {
	clauses := make([]postgres.OrderByClause, len(order.Fields))
	for i, field := range order.Fields {
		col := fields[field.Path].Column
		if field.Direction == aip.Desc {
			clauses[i] = col.DESC()
		} else {
			clauses[i] = col.ASC()
		}
	}
	return clauses
}

// buildKeysetCondition builds the WHERE clause that skips past already-seen
// rows (keyset/cursor pagination).
//
// For uniform-direction orderings (all ASC or all DESC), it uses PostgreSQL's
// native tuple comparison: ROW(col1, col2) > ROW(v1, v2).
// For mixed-direction orderings, it falls back to the traditional lexicographic
// OR-chain expansion.
func buildKeysetCondition[M any](order aip.OrderBy, vals []any, fields Fields[M]) (postgres.BoolExpression, error) {
	if len(vals) == 0 {
		return nil, nil //nolint:nilnil // No cursor is valid.
	}

	if len(order.Fields) == 0 {
		return nil, errors.New("no order fields provided")
	}

	if len(order.Fields) != len(vals) {
		return nil, errors.New("cursor/value length mismatch")
	}

	if aip.IsUniformDirection(order) {
		return buildTupleComparison(order, vals, fields)
	}

	return buildLexicographicFallback(order, vals, fields)
}

// buildTupleComparison builds a ROW(col1, col2) > ROW(val1, val2) expression
// for uniform-direction ordering.
func buildTupleComparison[M any](order aip.OrderBy, vals []any, fields Fields[M]) (postgres.BoolExpression, error) {
	cols := make([]postgres.Expression, len(order.Fields))
	litVals := make([]postgres.Expression, len(vals))

	for i, f := range order.Fields {
		cols[i] = fields[f.Path].Column

		lit, err := toLiteral(vals[i])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Path, err)
		}
		litVals[i] = lit
	}

	row := postgres.ROW(cols...)
	valRow := postgres.ROW(litVals...)

	if order.Fields[0].Direction == aip.Desc {
		return row.LT(valRow), nil
	}

	return row.GT(valRow), nil
}

// buildLexicographicFallback builds the traditional OR-chain keyset condition
// for mixed-direction orderings where tuple comparison cannot be used.
func buildLexicographicFallback[M any](order aip.OrderBy, vals []any, fields Fields[M]) (postgres.BoolExpression, error) {
	orConditions := make([]postgres.BoolExpression, 0, len(order.Fields))

	for i := range order.Fields {
		chain := make([]postgres.BoolExpression, 0, i+1)

		for j := range i {
			expr, err := equalityExpr(fields[order.Fields[j].Path].Column, vals[j])
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", order.Fields[j].Path, err)
			}
			chain = append(chain, expr)
		}

		cmp, err := directionExpr(fields[order.Fields[i].Path].Column, order.Fields[i].Direction, vals[i])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", order.Fields[i].Path, err)
		}

		chain = append(chain, cmp)
		orConditions = append(orConditions, postgres.AND(chain...))
	}

	return postgres.OR(orConditions...), nil
}

// combineConditions ANDs together non-nil conditions.
func combineConditions(conditions ...postgres.BoolExpression) postgres.BoolExpression {
	combined := make([]postgres.BoolExpression, 0, len(conditions))
	for _, condition := range conditions {
		if condition != nil {
			combined = append(combined, condition)
		}
	}
	if len(combined) == 0 {
		return nil
	}
	return postgres.AND(combined...)
}

// toLiteral converts a Go value into a go-jet Expression for use in ROW() comparisons.
func toLiteral(v any) (postgres.Expression, error) {
	switch val := v.(type) {
	case string:
		return postgres.String(val), nil
	case bool:
		return postgres.Bool(val), nil
	case int32:
		// Plugin-emitted schemas store INTEGER columns as int32 in the
		// jet model; the cursor codec surfaces them typed. Promote to
		// int64 for jet's literal builder.
		return postgres.Int64(int64(val)), nil
	case int64:
		return postgres.Int64(val), nil
	case float32:
		// REAL columns surface as float32 in the jet model; promote
		// to float64 so the literal builder has a single entry point.
		return postgres.Float(float64(val)), nil
	case float64:
		return postgres.Float(val), nil
	case time.Time:
		return postgres.TimestampzT(val), nil
	default:
		return nil, fmt.Errorf("toLiteral not implemented for %T", v)
	}
}

// equalityExpr builds a col = val expression for the lexicographic fallback.
func equalityExpr(col postgres.Column, v any) (postgres.BoolExpression, error) {
	switch val := v.(type) {
	case string:
		strCol, ok := col.(postgres.StringExpression)
		if !ok {
			return nil, errors.New("column is not a string expression")
		}
		return strCol.EQ(postgres.String(val)), nil
	case bool:
		boolCol, ok := col.(postgres.BoolExpression)
		if !ok {
			return nil, errors.New("column is not a bool expression")
		}
		return boolCol.EQ(postgres.Bool(val)), nil
	case int32:
		intCol, ok := col.(postgres.IntegerExpression)
		if !ok {
			return nil, errors.New("column is not an integer expression")
		}
		return intCol.EQ(postgres.Int64(int64(val))), nil
	case int64:
		intCol, ok := col.(postgres.IntegerExpression)
		if !ok {
			return nil, errors.New("column is not an integer expression")
		}
		return intCol.EQ(postgres.Int64(val)), nil
	case float32:
		floatCol, ok := col.(postgres.FloatExpression)
		if !ok {
			return nil, errors.New("column is not a float expression")
		}
		return floatCol.EQ(postgres.Float(float64(val))), nil
	case float64:
		floatCol, ok := col.(postgres.FloatExpression)
		if !ok {
			return nil, errors.New("column is not a float expression")
		}
		return floatCol.EQ(postgres.Float(val)), nil
	case time.Time:
		if timeCol, ok := col.(postgres.TimestampzExpression); ok {
			return timeCol.EQ(postgres.TimestampzT(val)), nil
		}
		if timeCol, ok := col.(postgres.TimestampExpression); ok {
			return timeCol.EQ(postgres.TimestampT(val)), nil
		}
		return nil, errors.New("column is not a timestamp(z) expression")
	default:
		return nil, fmt.Errorf("equality not implemented for %T", v)
	}
}

// directionExpr builds a col > val (ASC) or col < val (DESC) expression
// for the lexicographic fallback.
func directionExpr(col postgres.Column, dir aip.SortDirection, v any) (postgres.BoolExpression, error) {
	asc := dir == aip.Asc

	switch val := v.(type) {
	case string:
		strCol, ok := col.(postgres.StringExpression)
		if !ok {
			return nil, errors.New("column is not a string expression")
		}
		lit := postgres.String(val)
		if asc {
			return strCol.GT(lit), nil
		}
		return strCol.LT(lit), nil
	case int32:
		intCol, ok := col.(postgres.IntegerExpression)
		if !ok {
			return nil, errors.New("column is not an integer expression")
		}
		lit := postgres.Int64(int64(val))
		if asc {
			return intCol.GT(lit), nil
		}
		return intCol.LT(lit), nil
	case int64:
		intCol, ok := col.(postgres.IntegerExpression)
		if !ok {
			return nil, errors.New("column is not an integer expression")
		}
		lit := postgres.Int64(val)
		if asc {
			return intCol.GT(lit), nil
		}
		return intCol.LT(lit), nil
	case float32:
		floatCol, ok := col.(postgres.FloatExpression)
		if !ok {
			return nil, errors.New("column is not a float expression")
		}
		lit := postgres.Float(float64(val))
		if asc {
			return floatCol.GT(lit), nil
		}
		return floatCol.LT(lit), nil
	case float64:
		floatCol, ok := col.(postgres.FloatExpression)
		if !ok {
			return nil, errors.New("column is not a float expression")
		}
		lit := postgres.Float(val)
		if asc {
			return floatCol.GT(lit), nil
		}
		return floatCol.LT(lit), nil
	case time.Time:
		if timeCol, ok := col.(postgres.TimestampzExpression); ok {
			lit := postgres.TimestampzT(val)
			if asc {
				return timeCol.GT(lit), nil
			}
			return timeCol.LT(lit), nil
		}
		if timeCol, ok := col.(postgres.TimestampExpression); ok {
			lit := postgres.TimestampT(val)
			if asc {
				return timeCol.GT(lit), nil
			}
			return timeCol.LT(lit), nil
		}
		return nil, errors.New("column is not a timestamp(z) expression")
	default:
		return nil, fmt.Errorf("comparison not implemented for %T", v)
	}
}
