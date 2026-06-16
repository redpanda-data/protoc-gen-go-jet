// Package jet — filter translation.
//
// FilterToCondition turns an AIP-160 filter expression into a go-jet
// BoolExpression so repositories can push the filter into the SQL
// WHERE clause rather than filtering in-process after pagination.
// In-process filtering under cursor pagination is actively broken:
// a caller asking for page_size=50 with a filter matching 3 rows
// gets 3 items plus a next_page_token, no way to tell if more
// matches exist on later pages.
//
// AIP-160 spec: https://google.aip.dev/160
// EBNF grammar: https://google.aip.dev/assets/misc/ebnf-filtering.txt
//
// Supported constructs (subset of the full spec — extended on demand):
//
//	field = VALUE       — equality
//	field != VALUE      — inequality
//	field < VALUE       — strict less-than (ordered types only)
//	field <= VALUE      — less-than-or-equal
//	field > VALUE       — strict greater-than
//	field >= VALUE      — greater-than-or-equal
//	field : VALUE       — the "has" operator: substring match on strings
//	NOT expr, -expr     — logical negation
//	expr AND expr       — conjunction
//	expr OR expr        — disjunction
//	(expr)              — grouping
//
// Timestamp and duration literals work on the appropriate columns:
//
//	created_at >= timestamp("2025-01-01T00:00:00Z")  # ColumnTimestampz
//	ttl > duration("5m")                             # ColumnInteger (BIGINT ns)
//
// NULL predicates work against any column:
//
//	name = null   # IS NULL
//	name != null  # IS NOT NULL
//
// Dotted paths compile to JSONB accessors when the caller registers
// them in FilterFields via a StringExpression, e.g.:
//
//	fields := FilterFields{
//	  "id":                    Table.ID,
//	  "spec.install_pack":     postgres.RawString("spec->>'install_pack'"),
//	}
//	// filter: spec.install_pack = "24.1.1"
//
// The plugin auto-registers these for every column with
// jsonb_indexed_paths — callers rarely touch the map by hand.
//
// Not yet supported (returning an error beats silently returning the
// wrong rows — any caller can fall back to passing an empty filter):
//
//   - custom functions (only timestamp() and duration() are recognised)
//   - map traversal (list traversal on TEXT[] columns works — see below)
//   - wildcard literals in strings
//
// TEXT[] columns (repeated string, repeated enum-as-name) support a
// narrow containment subset:
//
//	zones = "us-west-1"   # 'us-west-1' = ANY(zones)
//	zones != "us-west-1"  # NOT ('us-west-1' = ANY(zones))
//	zones : "us-west-1"   # same as =, AIP-160 "has" on a list
//
// Ordering (`<`, `<=`, `>`, `>=`) on an array column is rejected — the
// semantics aren't meaningful. NULL predicates against the whole column
// work the same way as plain text (`zones = null` → `IS NULL`).
package jet

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"go.einride.tech/aip/filtering"
	expr "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// FilterFields binds AIP-160 field identifiers (the bare names used in
// the filter expression) to go-jet expressions the translator can emit
// SQL for. Values are typically go-jet `Column` instances, but dotted
// paths into JSONB columns are supported via `StringExpression`-typed
// entries (e.g. `postgres.RawString("config->>'api_key_ref'")`). The
// expression's static type determines which comparisons are valid —
// string expressions admit `:`, integer expressions do not.
//
// Column values remain compatible here because jet's `Column` interface
// is a subtype of `Expression` (ColumnExpression embeds Expression);
// existing callers that pass `Table.X` compile unchanged.
type FilterFields map[string]postgres.Expression

// ErrUnsupportedFilter is the sentinel for filter shapes the translator
// does not yet handle. Callers typically map it to InvalidArgument.
var ErrUnsupportedFilter = errors.New("unsupported AIP-160 filter shape")

// MaxFilterLength caps the accepted filter-expression length. The parser
// has no built-in depth or length bound, so an attacker could submit an
// arbitrarily large filter string and force the server to spend CPU and
// memory parsing it. 4 KiB is roughly 40× larger than any sensible
// real-world filter and still fits comfortably in a single packet.
// Callers that need more can wrap FilterToCondition and enforce their
// own limit; the default exists so no caller silently ships DoS.
const MaxFilterLength = 4096

// FilterToCondition parses an AIP-160 filter expression and returns a
// go-jet BoolExpression that can be ANDed into a SELECT's WHERE clause.
//
// An empty filter returns (nil, nil) meaning "no predicate". Syntax
// errors and unsupported shapes return a non-nil error wrapping
// ErrUnsupportedFilter so callers can distinguish user error from a
// programming bug.
//
// Pass the returned BoolExpression as the baseCondition argument of
// ExecuteWithCondition: the keyset cursor predicate is ANDed with it,
// so pagination remains correct under filtering.
func FilterToCondition(filter string, fields FilterFields) (postgres.BoolExpression, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return nil, nil
	}
	if len(filter) > MaxFilterLength {
		return nil, fmt.Errorf("%w: filter exceeds %d bytes", ErrUnsupportedFilter, MaxFilterLength)
	}
	var p filtering.Parser
	p.Init(filter)
	parsed, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("%w: parse %q: %w", ErrUnsupportedFilter, filter, err)
	}
	return translate(parsed.GetExpr(), fields)
}

// translate walks a CEL-shaped AIP-160 expression tree and produces the
// matching jet BoolExpression. The tree shapes it handles correspond
// to AIP-160's EBNF: Expression/Factor/Term map onto CallExpr nodes
// whose function name is one of the filtering.Function* constants.
func translate(e *expr.Expr, fields FilterFields) (postgres.BoolExpression, error) {
	call := e.GetCallExpr()
	if call == nil {
		// Bare identifier / literal at the top of a filter is only
		// meaningful for the predicate form `field` (boolean test),
		// which AIP-160 doesn't define at the top level. Anything else
		// is a programming error in the caller's filter string.
		return nil, fmt.Errorf("%w: expected boolean expression, got %T", ErrUnsupportedFilter, e.GetExprKind())
	}

	fn := call.GetFunction()
	args := call.GetArgs()

	switch fn {
	case filtering.FunctionAnd:
		return andOf(args, fields)
	case filtering.FunctionOr:
		return orOf(args, fields)
	case filtering.FunctionNot:
		if len(args) != 1 {
			return nil, fmt.Errorf("%w: NOT takes exactly one argument, got %d", ErrUnsupportedFilter, len(args))
		}
		inner, err := translate(args[0], fields)
		if err != nil {
			return nil, err
		}
		return postgres.NOT(inner), nil
	case filtering.FunctionEquals,
		filtering.FunctionNotEquals,
		filtering.FunctionLessThan,
		filtering.FunctionLessEquals,
		filtering.FunctionGreaterThan,
		filtering.FunctionGreaterEquals,
		filtering.FunctionHas:
		return comparison(fn, args, fields)
	default:
		return nil, fmt.Errorf("%w: function %q", ErrUnsupportedFilter, fn)
	}
}

// AIP-160 defines AND to bind tighter than OR, but the parser already
// encodes that in the tree, so we just fold linearly.
func andOf(args []*expr.Expr, fields FilterFields) (postgres.BoolExpression, error) {
	var acc postgres.BoolExpression
	for _, a := range args {
		term, err := translate(a, fields)
		if err != nil {
			return nil, err
		}
		if acc == nil {
			acc = term
		} else {
			acc = acc.AND(term)
		}
	}
	return acc, nil
}

func orOf(args []*expr.Expr, fields FilterFields) (postgres.BoolExpression, error) {
	var acc postgres.BoolExpression
	for _, a := range args {
		term, err := translate(a, fields)
		if err != nil {
			return nil, err
		}
		if acc == nil {
			acc = term
		} else {
			acc = acc.OR(term)
		}
	}
	return acc, nil
}

// comparison handles the binary operators `=`, `!=`, `<`, `<=`, `>`,
// `>=`, `:`. The left-hand side is a bare field identifier ("name")
// or a dotted path ("config.api_key_ref") declared in fields; the
// right-hand side is a literal.
func comparison(fn string, args []*expr.Expr, fields FilterFields) (postgres.BoolExpression, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%w: %s takes two arguments, got %d", ErrUnsupportedFilter, fn, len(args))
	}
	key, err := flattenIdent(args[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %s left-hand side: %s", ErrUnsupportedFilter, fn, err)
	}
	col, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("%w: unknown field %q (available: %s)",
			ErrUnsupportedFilter, key, availableFieldNames(fields))
	}
	return applyComparator(fn, col, args[1])
}

// flattenIdent renders a CEL identifier or member-access chain back to
// its dotted-string form. `name` stays `name`; `config.api_key_ref`
// comes back as the single key the caller registered; `a.b.c`
// flattens the same way. Any other expression shape (function calls,
// literals, index operations) is rejected — the filter grammar allows
// them on the RHS, not the LHS.
func flattenIdent(e *expr.Expr) (string, error) {
	switch k := e.GetExprKind().(type) {
	case *expr.Expr_IdentExpr:
		return k.IdentExpr.GetName(), nil
	case *expr.Expr_SelectExpr:
		head, err := flattenIdent(k.SelectExpr.GetOperand())
		if err != nil {
			return "", err
		}
		field := k.SelectExpr.GetField()
		if field == "" {
			return "", errors.New("empty field in member-access chain")
		}
		return head + "." + field, nil
	default:
		return "", fmt.Errorf("expected identifier or dotted path, got %T", k)
	}
}

// availableFieldNames returns the sorted comma-separated list of
// identifiers in the FilterFields map, for user-facing error messages.
// A caller who misspells a field gets the actual set in the error
// rather than guessing which typo they made.
func availableFieldNames(fields FilterFields) string {
	if len(fields) == 0 {
		return "<none>"
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// applyComparator dispatches on both the operator and the jet column
// type. Each concrete column type (ColumnString, ColumnBool, etc.)
// defines its own comparison methods — Go generics don't cover this
// cleanly, so the switch is the honest encoding.
func applyComparator(fn string, col postgres.Expression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	// NULL literal predicate works on every expression type without
	// needing a per-type op branch. Only = / != are valid against
	// NULL; ordering / substring comparisons return undefined on
	// NULL in SQL, so we reject them cleanly rather than generating
	// silently-empty result sets.
	if isNullLiteral(rhs) {
		switch fn {
		case filtering.FunctionEquals:
			return col.IS_NULL(), nil
		case filtering.FunctionNotEquals:
			return col.IS_NOT_NULL(), nil
		default:
			return nil, fmt.Errorf("%w: NULL literal is only valid with = or !=", ErrUnsupportedFilter)
		}
	}
	// Concrete Column types (ColumnString, ColumnInteger, ...) are
	// listed first so the switch picks them before the broader
	// *Expression fallbacks. The broader cases cover raw expressions
	// from JSONB dotted paths (`col->>'x'` — StringExpression, not a
	// ColumnString).
	switch c := col.(type) {
	case postgres.ColumnStringArray:
		// TEXT[] — emit `'x' = ANY(col)` containment. Kept above
		// ColumnString because ColumnStringArray embeds Array, which
		// is distinct from Column, so there's no overlap; ordering is
		// still cosmetic.
		return stringArrayOp(fn, c, rhs)
	case postgres.ColumnString:
		return stringOp(fn, c, rhs)
	case postgres.ColumnBool:
		return boolOp(fn, c, rhs)
	case postgres.ColumnInteger:
		return integerOp(fn, c, rhs)
	case postgres.ColumnFloat:
		return floatOp(fn, c, rhs)
	case postgres.ColumnTimestampz:
		return timestampzOp(fn, c, rhs)
	case postgres.StringExpression:
		// JSONB path accessor (->>'x') returns StringExpression. Treat
		// it exactly like a ColumnString — EQ/NOT_EQ/LT/.../LIKE all
		// exist on the StringExpression interface.
		return stringOp(fn, c, rhs)
	case postgres.BoolExpression:
		return boolOp(fn, c, rhs)
	case postgres.IntegerExpression:
		return integerOp(fn, c, rhs)
	case postgres.FloatExpression:
		return floatOp(fn, c, rhs)
	case postgres.TimestampzExpression:
		return timestampzOp(fn, c, rhs)
	default:
		return nil, fmt.Errorf("%w: expression type %T", ErrUnsupportedFilter, col)
	}
}

// isNullLiteral reports whether the RHS is the bare `null` token.
// AIP-160 reserves `null` as a keyword (EBNF §Keywords), so there's no
// ambiguity with a column identifier. CEL's parser path here emits
// either a Constant_NullValue (typed literal) or an IdentExpr named
// "null" depending on how the grammar resolves; accept both so the
// filter spelling doesn't leak through the parser's internal choice.
func isNullLiteral(e *expr.Expr) bool {
	if c := e.GetConstExpr(); c != nil {
		if _, ok := c.GetConstantKind().(*expr.Constant_NullValue); ok {
			return true
		}
	}
	if id := e.GetIdentExpr(); id != nil && id.GetName() == "null" {
		return true
	}
	return false
}

// stringArrayOp handles equality / inequality / `has` on a TEXT[]
// column. AIP-160's "has" semantics on a list are containment — the
// list HAS the element — which PG expresses as `value = ANY(col)`.
// Equality collapses to the same expression: `zones = "us-west-1"`
// is semantically "does the zones list contain 'us-west-1'", not
// "is the whole array exactly ['us-west-1']". The latter reading is
// what SQL `=` on arrays does, but it's a near-useless query in
// practice and contradicts what AIP-160 says about list membership.
//
// Inequality is the NOT of that — a row's zones list does NOT
// contain the needle. `NOT ('x' = ANY(col))` handles NULL elements
// consistently (SQL three-valued logic yields false rows).
//
// Ordering (<, <=, >, >=) is rejected. The underlying type is
// string-array so PG does define lexicographic comparison, but the
// semantics under filter grammar are ambiguous and almost always a
// caller bug.
func stringArrayOp(fn string, col postgres.ColumnStringArray, rhs *expr.Expr) (postgres.BoolExpression, error) {
	s, err := stringLiteral(rhs)
	if err != nil {
		return nil, err
	}
	elem := postgres.String(s).EQ(postgres.ANY(col))
	switch fn {
	case filtering.FunctionEquals, filtering.FunctionHas:
		return elem, nil
	case filtering.FunctionNotEquals:
		return postgres.NOT(elem), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on repeated-text column (only =, !=, : are meaningful)", ErrUnsupportedFilter, fn)
	}
}

func stringOp(fn string, col postgres.StringExpression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	s, err := stringLiteral(rhs)
	if err != nil {
		return nil, err
	}
	switch fn {
	case filtering.FunctionEquals:
		return col.EQ(postgres.String(s)), nil
	case filtering.FunctionNotEquals:
		return col.NOT_EQ(postgres.String(s)), nil
	case filtering.FunctionLessThan:
		return col.LT(postgres.String(s)), nil
	case filtering.FunctionLessEquals:
		return col.LT_EQ(postgres.String(s)), nil
	case filtering.FunctionGreaterThan:
		return col.GT(postgres.String(s)), nil
	case filtering.FunctionGreaterEquals:
		return col.GT_EQ(postgres.String(s)), nil
	case filtering.FunctionHas:
		// AIP-160 defines `:` on a string LHS as substring match.
		// https://google.aip.dev/160#literals — "has" is the
		// operator token; for scalar string fields the semantics
		// collapse to "contains". Use a case-insensitive LIKE so
		// `name:"foo"` matches "Foo Bar" the way operators expect.
		//
		// Escape LIKE metacharacters (% _ \) in the needle before
		// wrapping — otherwise a caller filter like `name:"%"`
		// silently becomes "match any row" instead of "contains a
		// literal percent". Same privacy concern with `_`.
		pattern := "%" + escapeLike(strings.ToLower(s)) + "%"
		return postgres.LOWER(col).LIKE(postgres.String(pattern)), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on string column", ErrUnsupportedFilter, fn)
	}
}

func boolOp(fn string, col postgres.BoolExpression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	// AIP-160 treats `true` and `false` as bare identifiers in the
	// grammar — type resolution happens downstream in the checker.
	// We never run the checker (we don't have a type-declared schema
	// for the caller's filter), so accept the identifier form here.
	b, err := boolLiteral(rhs)
	if err != nil {
		return nil, err
	}
	switch fn {
	case filtering.FunctionEquals:
		return col.EQ(postgres.Bool(b)), nil
	case filtering.FunctionNotEquals:
		return col.NOT_EQ(postgres.Bool(b)), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on bool column", ErrUnsupportedFilter, fn)
	}
}

func boolLiteral(e *expr.Expr) (bool, error) {
	if ident := e.GetIdentExpr(); ident != nil {
		switch ident.GetName() {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	if cons := e.GetConstExpr(); cons != nil {
		if bv, ok := cons.GetConstantKind().(*expr.Constant_BoolValue); ok {
			return bv.BoolValue, nil
		}
	}
	return false, fmt.Errorf("%w: expected bool literal (true/false)", ErrUnsupportedFilter)
}

func timestampzOp(fn string, col postgres.TimestampzExpression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	t, err := timestampLiteral(rhs)
	if err != nil {
		return nil, err
	}
	lit := postgres.TimestampzT(t)
	switch fn {
	case filtering.FunctionEquals:
		return col.EQ(lit), nil
	case filtering.FunctionNotEquals:
		return col.NOT_EQ(lit), nil
	case filtering.FunctionLessThan:
		return col.LT(lit), nil
	case filtering.FunctionLessEquals:
		return col.LT_EQ(lit), nil
	case filtering.FunctionGreaterThan:
		return col.GT(lit), nil
	case filtering.FunctionGreaterEquals:
		return col.GT_EQ(lit), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on timestamp column", ErrUnsupportedFilter, fn)
	}
}

// integerLiteral accepts either a bare int literal or an AIP-160
// `duration("...")` call, in which case the returned int64 is the
// duration expressed as nanoseconds — matching KindDuration's storage
// shape (BIGINT ns in SQL, time.Duration ns at the Go layer).
func integerLiteral(e *expr.Expr) (int64, error) {
	if call := e.GetCallExpr(); call != nil {
		if call.GetFunction() != filtering.FunctionDuration {
			return 0, fmt.Errorf("%w: integer column rhs %q not supported (accepted: int literal, duration())", ErrUnsupportedFilter, call.GetFunction())
		}
		args := call.GetArgs()
		if len(args) != 1 {
			return 0, fmt.Errorf("%w: duration() takes one argument, got %d", ErrUnsupportedFilter, len(args))
		}
		s, err := stringLiteral(args[0])
		if err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%w: duration(%q): %w", ErrUnsupportedFilter, s, err)
		}
		return int64(d), nil
	}
	cons := e.GetConstExpr()
	if cons == nil {
		return 0, fmt.Errorf("%w: expected int or duration() literal", ErrUnsupportedFilter)
	}
	i, ok := cons.GetConstantKind().(*expr.Constant_Int64Value)
	if !ok {
		return 0, fmt.Errorf("%w: integer column rhs must be an int literal", ErrUnsupportedFilter)
	}
	return i.Int64Value, nil
}

// timestampLiteral parses an AIP-160 `timestamp("...")` call into a
// Go time.Time. AIP-160 defers to RFC3339 for the string form; see
// https://google.aip.dev/160#literals — "Timestamps follow the
// RFC-3339 specification".
func timestampLiteral(e *expr.Expr) (time.Time, error) {
	call := e.GetCallExpr()
	if call == nil || call.GetFunction() != filtering.FunctionTimestamp {
		return time.Time{}, fmt.Errorf("%w: expected timestamp(\"...\") literal", ErrUnsupportedFilter)
	}
	args := call.GetArgs()
	if len(args) != 1 {
		return time.Time{}, fmt.Errorf("%w: timestamp() takes one argument, got %d", ErrUnsupportedFilter, len(args))
	}
	s, err := stringLiteral(args[0])
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: timestamp(%q): %w", ErrUnsupportedFilter, s, err)
	}
	return t, nil
}

// floatOp handles the binary comparators on REAL / DOUBLE PRECISION
// columns. Mirrors integerOp structurally — the difference is the
// literal form: AIP-160 floats come through the parser as
// ConstExpr_DoubleValue, and go-jet renders them via
// `postgres.Float`.
//
// `has` (`:`) isn't defined for float columns — substring match on a
// float column is nonsense, and CEL's grammar would require a string
// literal anyway.
func floatOp(fn string, col postgres.FloatExpression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	f, err := floatLiteral(rhs)
	if err != nil {
		return nil, err
	}
	switch fn {
	case filtering.FunctionEquals:
		return col.EQ(postgres.Float(f)), nil
	case filtering.FunctionNotEquals:
		return col.NOT_EQ(postgres.Float(f)), nil
	case filtering.FunctionLessThan:
		return col.LT(postgres.Float(f)), nil
	case filtering.FunctionLessEquals:
		return col.LT_EQ(postgres.Float(f)), nil
	case filtering.FunctionGreaterThan:
		return col.GT(postgres.Float(f)), nil
	case filtering.FunctionGreaterEquals:
		return col.GT_EQ(postgres.Float(f)), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on float column", ErrUnsupportedFilter, fn)
	}
}

// floatLiteral extracts a float64 from an AIP-160 numeric literal.
// Accepts either a CEL DoubleValue (written with a decimal point or
// exponent) or an Int64Value (bare whole number — the grammar has no
// way to disambiguate `42` from `42.0` at parse time, so a float
// column filter like `price > 42` needs to accept ints too).
func floatLiteral(e *expr.Expr) (float64, error) {
	cons := e.GetConstExpr()
	if cons == nil {
		return 0, fmt.Errorf("%w: expected numeric literal, got %T", ErrUnsupportedFilter, e.GetExprKind())
	}
	switch v := cons.GetConstantKind().(type) {
	case *expr.Constant_DoubleValue:
		return v.DoubleValue, nil
	case *expr.Constant_Int64Value:
		return float64(v.Int64Value), nil
	case *expr.Constant_Uint64Value:
		return float64(v.Uint64Value), nil
	}
	return 0, fmt.Errorf("%w: expected numeric literal, got %T", ErrUnsupportedFilter, cons.GetConstantKind())
}

func integerOp(fn string, col postgres.IntegerExpression, rhs *expr.Expr) (postgres.BoolExpression, error) {
	// Accept either a bare int literal or duration("...") → nanoseconds.
	// Integer columns that store durations (Kind=KindDuration in the
	// plugin's ColumnPlan) get the BIGINT-ns representation, so
	// `ttl > duration("5m")` compiles to `ttl > 300000000000`.
	n, err := integerLiteral(rhs)
	if err != nil {
		return nil, err
	}
	switch fn {
	case filtering.FunctionEquals:
		return col.EQ(postgres.Int(n)), nil
	case filtering.FunctionNotEquals:
		return col.NOT_EQ(postgres.Int(n)), nil
	case filtering.FunctionLessThan:
		return col.LT(postgres.Int(n)), nil
	case filtering.FunctionLessEquals:
		return col.LT_EQ(postgres.Int(n)), nil
	case filtering.FunctionGreaterThan:
		return col.GT(postgres.Int(n)), nil
	case filtering.FunctionGreaterEquals:
		return col.GT_EQ(postgres.Int(n)), nil
	default:
		return nil, fmt.Errorf("%w: %s not valid on integer column", ErrUnsupportedFilter, fn)
	}
}

// escapeLike escapes the three metacharacters Postgres LIKE treats
// specially — `%` (any sequence), `_` (any single char), and `\` (the
// escape char). The substitution uses `\` as the escape character,
// which is Postgres' default for LIKE. Source: PostgreSQL docs §9.7.1
// "LIKE". https://www.postgresql.org/docs/current/functions-matching.html#FUNCTIONS-LIKE
//
// Order matters: escape `\` first so we don't double-escape the
// backslashes we introduce for `%` and `_`.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// stringLiteral extracts a Go string from a const expression. AIP-160
// strings can be single-quoted or double-quoted; the parser normalises
// both into a ConstExpr with a StringValue.
func stringLiteral(e *expr.Expr) (string, error) {
	cons := e.GetConstExpr()
	if cons == nil {
		return "", fmt.Errorf("%w: expected string literal, got %T", ErrUnsupportedFilter, e.GetExprKind())
	}
	sv, ok := cons.GetConstantKind().(*expr.Constant_StringValue)
	if !ok {
		return "", fmt.Errorf("%w: expected string literal, got %T", ErrUnsupportedFilter, cons.GetConstantKind())
	}
	return sv.StringValue, nil
}
