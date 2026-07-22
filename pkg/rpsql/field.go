package rpsql

import (
	"fmt"
	"regexp"

	"github.com/go-jet/jet/v2/postgres"
)

// identRe matches an unquoted SQL identifier. Composite member names are
// interpolated into the SQL text (they are not bind parameters), so Field
// rejects anything that is not a plain identifier to keep the surface
// injection-free.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Field builds Redpanda SQL composite/struct member access: (col).member.
//
// Redpanda SQL resolves a composite column's members with the parenthesised form
// (col).member; the bare form col.member is instead parsed as table.column.
// go-jet's postgres dialect emits the bare form, so Field is required to read
// members of a composite (record) column.
//
// The returned expression is untyped; prefer the typed helpers (FieldString,
// FieldInt, ...) so the value can be used in typed comparisons, ORDER BY and
// projections. Field panics if member is not a plain SQL identifier.
func Field(col postgres.Expression, member string) postgres.Expression {
	if !identRe.MatchString(member) {
		panic(fmt.Sprintf("rpsql: invalid composite member name %q", member))
	}
	// CustomExpression wraps its parts in parentheses, so the inner
	// CustomExpression(col) yields (col) and the outer appends .member. Depending
	// on context go-jet may or may not add an extra outer pair, giving either
	// (col).member or ((col).member) — both are valid, equivalent Redpanda SQL.
	return postgres.CustomExpression(postgres.CustomExpression(col), postgres.Token("."+member))
}

// FieldString accesses a text-typed composite member.
func FieldString(col postgres.Expression, member string) postgres.StringExpression {
	return postgres.StringExp(Field(col, member))
}

// FieldInt accesses an integer-typed composite member.
func FieldInt(col postgres.Expression, member string) postgres.IntegerExpression {
	return postgres.IntExp(Field(col, member))
}

// FieldBool accesses a boolean-typed composite member.
func FieldBool(col postgres.Expression, member string) postgres.BoolExpression {
	return postgres.BoolExp(Field(col, member))
}

// FieldFloat accesses a floating-point / numeric composite member.
func FieldFloat(col postgres.Expression, member string) postgres.FloatExpression {
	return postgres.FloatExp(Field(col, member))
}

// FieldTimestampz accesses a timestamptz composite member.
func FieldTimestampz(col postgres.Expression, member string) postgres.TimestampzExpression {
	return postgres.TimestampzExp(Field(col, member))
}

// FieldTimestamp accesses a timestamp (without time zone) composite member.
func FieldTimestamp(col postgres.Expression, member string) postgres.TimestampExpression {
	return postgres.TimestampExp(Field(col, member))
}

// FieldDate accesses a date composite member.
func FieldDate(col postgres.Expression, member string) postgres.DateExpression {
	return postgres.DateExp(Field(col, member))
}
