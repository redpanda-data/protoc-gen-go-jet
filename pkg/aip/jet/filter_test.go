package jet_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aipjet "github.com/redpanda-data/protoc-gen-go-jet/pkg/aip/jet"
)

// quoteForFilter wraps a literal needle in an AIP-160 double-quoted
// string, escaping backslashes per the spec's GoogleSQL-style rules.
// The filter parser unescapes `\\` → `\`.
func quoteForFilter(needle string) string {
	return `"` + strings.ReplaceAll(needle, `\`, `\\`) + `"`
}

// Fixture tables. The columns mirror realistic resource schemas closely
// enough that tests read as real filter expressions — the same grammar
// a client would send.
//
// AIP-160 EBNF reference:
//
//	https://google.aip.dev/assets/misc/ebnf-filtering.txt
var (
	nameCol      = postgres.StringColumn("name")
	displayCol   = postgres.StringColumn("display_name")
	enabledCol   = postgres.BoolColumn("enabled")
	priorityCol  = postgres.IntegerColumn("priority")
	scoreCol     = postgres.FloatColumn("score")
	createdAtCol = postgres.TimestampzColumn("created_at")
	zonesCol     = postgres.StringArrayColumn("zones")

	providers = postgres.NewTable("public", "providers", "",
		nameCol, displayCol, enabledCol, priorityCol, scoreCol, createdAtCol, zonesCol)
)

func fields() aipjet.FilterFields {
	return aipjet.FilterFields{
		"name":         nameCol,
		"display_name": displayCol,
		"enabled":      enabledCol,
		"priority":     priorityCol,
		"score":        scoreCol,
		"created_at":   createdAtCol,
		"zones":        zonesCol,
	}
}

// sqlFor composes a SELECT ... FROM providers WHERE <cond> so we can
// serialise the BoolExpression to an SQL string for comparison. The
// SELECT wrapping is necessary because BoolExpression alone has no
// public serialisation method in go-jet's public API.
func sqlFor(t *testing.T, cond postgres.BoolExpression) string {
	t.Helper()
	stmt := postgres.SELECT(nameCol).FROM(providers).WHERE(cond)
	// DebugSql inlines literals so assertions can spell them out;
	// Sql() returns $N placeholders, useful at runtime but opaque here.
	return stmt.DebugSql()
}

// TestFilterToCondition_Empty exercises the AIP-160 §"Request Filter"
// contract: an empty filter applies no predicate.
// Ref: https://google.aip.dev/160#request-filter-field
func TestFilterToCondition_Empty(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "   ", "\t\n"} {
		cond, err := aipjet.FilterToCondition(input, fields())
		require.NoError(t, err)
		assert.Nil(t, cond, "empty filter %q must produce no predicate", input)
	}
}

// TestFilterToCondition_Equality exercises AIP-160 §"Equality", the
// core `field = value` form.
// Ref: https://google.aip.dev/160#equality
func TestFilterToCondition_Equality(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		filter  string
		wantSQL string
	}{
		{
			name:    "string equals",
			filter:  `name = "openai"`,
			wantSQL: `providers.name = 'openai'::text`,
		},
		{
			name:    "string not equals",
			filter:  `name != "openai"`,
			wantSQL: `providers.name != 'openai'::text`,
		},
		{
			name:    "bool equals true",
			filter:  `enabled = true`,
			wantSQL: `providers.enabled = TRUE`,
		},
		{
			name:    "bool equals false",
			filter:  `enabled = false`,
			wantSQL: `providers.enabled = FALSE`,
		},
		{
			name:    "integer equals",
			filter:  `priority = 5`,
			wantSQL: `providers.priority = 5`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, tt.wantSQL, "SQL: %s", sql)
		})
	}
}

// TestFilterToCondition_Comparison exercises AIP-160 §"Comparison", the
// ordered operators `<`, `<=`, `>`, `>=` on both integer and string
// columns. Strings compare lexically in PG — valid and occasionally
// useful for pagination-style `name >= "foo"` bound filters.
// Ref: https://google.aip.dev/160#comparison
func TestFilterToCondition_Comparison(t *testing.T) {
	t.Parallel()
	tests := []struct {
		filter  string
		wantSQL string
	}{
		{filter: `priority < 10`, wantSQL: `providers.priority < 10`},
		{filter: `priority <= 10`, wantSQL: `providers.priority <= 10`},
		{filter: `priority > 10`, wantSQL: `providers.priority > 10`},
		{filter: `priority >= 10`, wantSQL: `providers.priority >= 10`},
		{filter: `name > "a"`, wantSQL: `providers.name > 'a'::text`},
		{filter: `name < "z"`, wantSQL: `providers.name < 'z'::text`},
		{filter: `name >= "m"`, wantSQL: `providers.name >= 'm'::text`},
		{filter: `name <= "m"`, wantSQL: `providers.name <= 'm'::text`},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, tt.wantSQL, "SQL: %s", sql)
		})
	}
}

// TestFilterToCondition_Has exercises AIP-160 §"Traversal" `:` operator.
// On a scalar string column the semantics collapse to substring match;
// we emit a case-insensitive LIKE so `name:"foo"` matches "Foo Bar".
// Ref: https://google.aip.dev/160#traversal
func TestFilterToCondition_Has(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name:"openai"`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, `LOWER(providers.name) LIKE '%openai%'`, "SQL: %s", sql)
}

// TestFilterToCondition_HasLowercasesNeedle confirms the needle is
// downcased before the LIKE, so uppercase input still matches
// mixed-case rows.
func TestFilterToCondition_HasLowercasesNeedle(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name:"OpenAI"`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, `LOWER(providers.name) LIKE '%openai%'`, "SQL: %s", sql)
}

// TestFilterToCondition_RepeatedText exercises the TEXT[] path.
// `zones = "us-west-1"` must compile to `'us-west-1' = ANY(zones)`
// — PG's canonical containment predicate. Equality on an array
// column is deliberately not SQL-equality-on-arrays; AIP-160 "has"
// semantics on a list always mean containment.
func TestFilterToCondition_RepeatedText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		filter string
		want   string
	}{
		{
			name:   "equals - containment",
			filter: `zones = "us-west-1"`,
			want:   `'us-west-1'::text = ANY(providers.zones)`,
		},
		{
			name:   "has operator - same as equals on a list LHS",
			filter: `zones : "us-east-1"`,
			want:   `'us-east-1'::text = ANY(providers.zones)`,
		},
		{
			name:   "not equals - NOT of containment",
			filter: `zones != "eu-central-1"`,
			want:   `NOT ('eu-central-1'::text = ANY(providers.zones))`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, tt.want, "SQL: %s", sql)
		})
	}
}

// TestFilterToCondition_RepeatedText_NullPredicate pins NULL predicates
// against a TEXT[] column. Columns with a DEFAULT '{}' never store NULL
// in practice, but the filter must still compile the predicate without
// rejecting at the array-op dispatch — NULL handling belongs to the
// top-level applyComparator branch, which short-circuits before the
// type switch.
func TestFilterToCondition_RepeatedText_NullPredicate(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`zones = null`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "providers.zones IS NULL", "SQL: %s", sql)
}

// TestFilterToCondition_RepeatedText_OrderingRejected confirms the
// ordering operators (<, <=, >, >=) are rejected on a TEXT[] column.
// PG does define lexicographic ordering on arrays but the semantics
// under AIP-160 are ambiguous and almost always a caller bug — reject
// with a clear message rather than silently emit a query that works
// but doesn't match intent.
func TestFilterToCondition_RepeatedText_OrderingRejected(t *testing.T) {
	t.Parallel()
	for _, filter := range []string{
		`zones > "us-east-1"`,
		`zones < "us-east-1"`,
		`zones >= "us-east-1"`,
		`zones <= "us-east-1"`,
	} {
		t.Run(filter, func(t *testing.T) {
			t.Parallel()
			_, err := aipjet.FilterToCondition(filter, fields())
			require.Error(t, err)
			assert.True(t, errors.Is(err, aipjet.ErrUnsupportedFilter))
			assert.Contains(t, err.Error(), "repeated-text column")
		})
	}
}

// TestFilterToCondition_Conjunction exercises AIP-160 §"Conjunctions"
// — implicit/explicit AND binds tighter than OR.
// Ref: https://google.aip.dev/160#conjunctions
func TestFilterToCondition_Conjunction(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name = "openai" AND enabled = true`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "providers.name = 'openai'::text", "SQL: %s", sql)
	assert.Contains(t, sql, "providers.enabled = TRUE", "SQL: %s", sql)
	assert.Contains(t, sql, " AND ", "SQL must contain AND: %s", sql)
}

// TestFilterToCondition_Disjunction exercises AIP-160 §"Disjunctions"
// — explicit OR.
// Ref: https://google.aip.dev/160#disjunctions
func TestFilterToCondition_Disjunction(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name = "openai" OR name = "anthropic"`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "providers.name = 'openai'::text", "SQL: %s", sql)
	assert.Contains(t, sql, "providers.name = 'anthropic'::text", "SQL: %s", sql)
	assert.Contains(t, sql, " OR ", "SQL must contain OR: %s", sql)
}

// TestFilterToCondition_Negation exercises AIP-160 §"Negation".
// Both `NOT expr` and `-expr` are accepted by the parser.
// Ref: https://google.aip.dev/160#negation-operators
func TestFilterToCondition_Negation(t *testing.T) {
	t.Parallel()
	for _, filter := range []string{`NOT enabled = true`, `-(enabled = true)`} {
		t.Run(filter, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(filter, fields())
			require.NoError(t, err, "input %q", filter)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, "NOT ", "SQL must contain NOT: %s", sql)
			assert.Contains(t, sql, "providers.enabled = TRUE", "SQL: %s", sql)
		})
	}
}

// TestFilterToCondition_NilFields guards against a programming error
// on the calling side — if the caller passes nil Fields, every field
// identifier fails to resolve and errors loudly rather than silently
// returning unfiltered rows (which would be a correctness bug on a
// filter that looked like it was applied).
func TestFilterToCondition_NilFields(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`name = "x"`, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_PrecedenceOrBindsTighter pins AIP-160's
// surprising precedence: the EBNF defines expression as AND-separated
// sequences, with OR living inside each sequence. Net effect: OR binds
// tighter than AND. `a OR b AND c` parses as `(a OR b) AND c`.
//
// Ref (EBNF):
//
//	  https://google.aip.dev/assets/misc/ebnf-filtering.txt
//
//		expression
//		  : sequence {WS AND WS sequence}
//		  ;
//		factor
//		  : term {WS OR WS term}
//		  ;
//
// The spec's prose at https://google.aip.dev/160#conjunctions reads as
// if AND takes precedence; it doesn't. The EBNF is authoritative and
// the einride parser we use follows it. Tests pinning this keep future
// readers from "fixing" it the wrong way.
func TestFilterToCondition_PrecedenceOrBindsTighter(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(
		`name = "a" OR name = "b" AND enabled = true`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	// Expect the OR cluster to be grouped as a single term, with the
	// outer AND combining it against the enabled check.
	assert.Contains(t, sql, "(providers.name = 'a'::text) OR (providers.name = 'b'::text)",
		"OR must bind tighter than AND, SQL: %s", sql)
	assert.Contains(t, sql, " AND ", "AND must be the outer operator, SQL: %s", sql)
}

// TestFilterToCondition_ExplicitGrouping verifies that parenthesised
// groups override the default precedence.
// Ref: https://google.aip.dev/160#composite-expressions
func TestFilterToCondition_ExplicitGrouping(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(
		`(name = "a" OR name = "b") AND enabled = true`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	// Explicit grouping and default precedence produce the same shape
	// for this expression — proves the parser handles parens without
	// breaking anything else.
	assert.Contains(t, sql, "(providers.name = 'a'::text) OR (providers.name = 'b'::text)",
		"OR cluster must be grouped, SQL: %s", sql)
	assert.Contains(t, sql, " AND ", "AND must appear at the outer level, SQL: %s", sql)
}

// TestFilterToCondition_UnknownField rejects filters that reference
// fields the caller did not declare. Better to fail loud than return
// the wrong rows.
//
// The error also names every field the caller IS allowed to use,
// alphabetically — enumerating the options beats leaving the user to
// guess at a typo. `ghost` in the test below would get a list like
// `(available: created_at, display_name, enabled, name, priority)`.
func TestFilterToCondition_UnknownField(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`ghost = "x"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
	assert.Contains(t, err.Error(), `unknown field "ghost"`)
	assert.Contains(t, err.Error(), "available: ")
	// Sorted, alphabetical: first and last entries pinned.
	assert.Contains(t, err.Error(), "created_at,")
	assert.Contains(t, err.Error(), "priority")
}

// TestFilterToCondition_EmptyFieldsListedAsNone — if the caller passes
// an empty (but non-nil) FilterFields map, the error still guides them
// rather than showing an empty "available:" list that would read weird.
func TestFilterToCondition_EmptyFieldsListedAsNone(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`name = "x"`, aipjet.FilterFields{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<none>")
}

// TestFilterToCondition_InvalidOperatorForColumn rejects a comparison
// that doesn't make sense for the column's static type — e.g. `<` on
// a bool column.
func TestFilterToCondition_InvalidOperatorForColumn(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`enabled > true`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_TypeMismatch rejects a type-mismatched literal.
func TestFilterToCondition_TypeMismatch(t *testing.T) {
	t.Parallel()
	// Feeding a string literal into a bool column.
	_, err := aipjet.FilterToCondition(`enabled = "true"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_ParseError rejects syntactically invalid input.
func TestFilterToCondition_ParseError(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`name = `, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_ErrorsWrapSentinel ensures every error path
// wraps ErrUnsupportedFilter so callers can map uniformly to
// InvalidArgument.
func TestFilterToCondition_ErrorsWrapSentinel(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"bogus",                // parse error
		`ghost = "x"`,          // unknown field
		`enabled > true`,       // wrong op for bool
		`priority = "not-int"`, // wrong literal type
		`name = "a" AND ghost`, // nested unknown field, trailing bare ident
	} {
		t.Run(input, func(t *testing.T) {
			_, err := aipjet.FilterToCondition(input, fields())
			require.Error(t, err, "input %q must error", input)
			assert.True(t, errors.Is(err, aipjet.ErrUnsupportedFilter),
				"error for %q must wrap ErrUnsupportedFilter: %v", input, err)
		})
	}
}

// TestFilterToCondition_DurationLiteral exercises the duration() AIP-160
// literal on an integer column that stores a duration as BIGINT
// nanoseconds (the plugin's KindDuration shape). Ref:
// https://google.aip.dev/160#literals — "Durations follow the
// specification set out by google.protobuf.Duration".
func TestFilterToCondition_DurationLiteral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		filter string
		wantNs int64
	}{
		{filter: `priority > duration("5m")`, wantNs: int64(5 * 60 * 1e9)},
		{filter: `priority >= duration("1h")`, wantNs: int64(3600 * 1e9)},
		{filter: `priority = duration("500ms")`, wantNs: int64(500 * 1e6)},
		{filter: `priority < duration("1.5s")`, wantNs: int64(1500 * 1e6)},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, fmt.Sprintf("%d", tt.wantNs), "SQL: %s", sql)
		})
	}
}

func TestFilterToCondition_DurationRejectsBad(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`priority > duration("not-a-duration")`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

func TestFilterToCondition_DurationRejectsNoArg(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`priority > duration()`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_IntegerRejectsUnknownFunction confirms that
// integer columns don't quietly accept every call expression — only
// duration() is special-cased; anything else errors with the accepted
// set spelled out.
func TestFilterToCondition_IntegerRejectsUnknownFunction(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`priority > timestamp("2025-01-01T00:00:00Z")`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_TimestampComparison exercises timestamp()
// literals per AIP-160 §"Literals" — RFC 3339 strings wrapped in a
// timestamp() call. Ref: https://google.aip.dev/160#literals
func TestFilterToCondition_TimestampComparison(t *testing.T) {
	t.Parallel()
	// Jet serialises time literals as 'YYYY-MM-DD HH:MM:SS[.nnnnn]TZ'
	// (space between date and time, not the RFC3339 `T`). That's a
	// valid PG literal and the same shape jet uses everywhere else.
	tests := []struct {
		filter string
		want   string
	}{
		{
			filter: `created_at >= timestamp("2025-01-01T00:00:00Z")`,
			want:   `providers.created_at >= '2025-01-01 00:00:00Z'::timestamp with time zone`,
		},
		{
			filter: `created_at < timestamp("2025-12-31T23:59:59Z")`,
			want:   `providers.created_at < '2025-12-31 23:59:59Z'::timestamp with time zone`,
		},
		{
			filter: `created_at = timestamp("2025-06-15T12:00:00+02:00")`,
			want:   `providers.created_at = '2025-06-15 12:00:00+02:00'::timestamp with time zone`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, tt.want, "SQL: %s", sql)
		})
	}
}

// TestFilterToCondition_TimestampAllOperators pins every ordered
// comparison on a timestamp column. The earlier test covered `=`,
// `<`, `>=` and one `+zone` variant — this fills in `!=`, `<=`, `>`
// so timestampzOp's complete decision table is locked down.
func TestFilterToCondition_TimestampAllOperators(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"!=", "<=", ">"} {
		filter := `created_at ` + fn + ` timestamp("2025-01-01T00:00:00Z")`
		t.Run(filter, func(t *testing.T) {
			cond, err := aipjet.FilterToCondition(filter, fields())
			require.NoError(t, err, "filter %q", filter)
			sql := sqlFor(t, cond)
			// Relies on jet serialising the operator verbatim and the
			// date with a space separator.
			assert.Contains(t, sql, "providers.created_at "+fn+" '2025-01-01 00:00:00Z'",
				"filter %q → SQL %s", filter, sql)
		})
	}
}

// TestFilterToCondition_TimestampRejectsHas rejects the `:` operator on
// a timestamp column — timestamps have no "contains" semantic. The
// translator's per-column dispatch routes `:` to timestampzOp's default
// arm, which fails loud rather than emitting a nonsense LIKE.
func TestFilterToCondition_TimestampRejectsHas(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`created_at:"2025"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_TimestampRejectsBadRFC3339 locks in that the
// parser surfaces the parse error rather than silently substituting
// the zero time or swallowing the argument.
func TestFilterToCondition_TimestampRejectsBadRFC3339(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`created_at >= timestamp("not-a-time")`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_TimestampRejectsStringLiteral guards against
// a caller passing a bare string where timestamp() is expected.
// Without this check `created_at >= "2025-01-01..."` would hit the
// default timestampz-rhs path and blow up in a confusing way.
func TestFilterToCondition_TimestampRejectsStringLiteral(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`created_at >= "2025-01-01T00:00:00Z"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}

// TestFilterToCondition_TimestampInComposite confirms timestamp() can
// be combined with AND/OR like any other predicate.
func TestFilterToCondition_TimestampInComposite(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(
		`enabled = true AND created_at >= timestamp("2025-01-01T00:00:00Z")`,
		fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "providers.enabled = TRUE")
	assert.Contains(t, sql, `providers.created_at >= '2025-01-01 00:00:00Z'::timestamp with time zone`)
	assert.Contains(t, sql, " AND ")
}

// TestFilterToCondition_HasEscapesLikeMetacharacters is the security
// regression guard for the `:` operator. A caller filter of `name:"%"`
// must match literal percent, not every row. Same for `_` (single-char
// wildcard) and `\` (LIKE's escape char). Without escaping, a filter
// becomes a privacy bypass: asking for "contains %" silently returns
// the whole table.
//
// The escape uses `\` as Postgres' default LIKE escape character —
// see https://www.postgresql.org/docs/current/functions-matching.html
func TestFilterToCondition_HasEscapesLikeMetacharacters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		needle string
		want   string
	}{
		{name: "percent", needle: "%", want: `%\%%`},
		{name: "underscore", needle: "_", want: `%\_%`},
		{name: "backslash", needle: `\`, want: `%\\%`},
		{name: "mixed", needle: `50%_done`, want: `%50\%\_done%`},
		{name: "no-meta", needle: "openai", want: `%openai%`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := `name:` + quoteForFilter(tt.needle)
			cond, err := aipjet.FilterToCondition(filter, fields())
			require.NoError(t, err, "filter %q", filter)
			sql := sqlFor(t, cond)
			assert.Contains(t, sql, tt.want, "SQL must contain escaped LIKE pattern: %s", sql)
		})
	}
}

// TestFilterToCondition_RejectsOversizedFilter is the DoS guard.
// einride's parser has no length bound; without the cap a malicious
// caller could spend server CPU and memory parsing a huge filter.
func TestFilterToCondition_RejectsOversizedFilter(t *testing.T) {
	t.Parallel()
	// Construct a filter just over the limit using a long literal.
	// The literal is a single AIP-160 token so the parser would
	// happily accept it if we didn't cap upfront.
	oversized := `name:"` + strings.Repeat("a", aipjet.MaxFilterLength) + `"`
	require.Greater(t, len(oversized), aipjet.MaxFilterLength)
	_, err := aipjet.FilterToCondition(oversized, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestFilterToCondition_CompositeMatchesRealUseCase — the scenario the
// reviewer flagged: "name contains openai AND enabled". Confirms the
// service's translated filter produces the expected SQL.
func TestFilterToCondition_CompositeMatchesRealUseCase(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name:"openai" AND enabled = true`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, `LOWER(providers.name) LIKE '%openai%'`, "SQL: %s", sql)
	assert.Contains(t, sql, `providers.enabled = TRUE`, "SQL: %s", sql)
	assert.Contains(t, sql, " AND ", "SQL: %s", sql)
}

// TestFilterToCondition_NullLiteral pins the NULL predicate path —
// `field = null` compiles to `IS NULL`, `field != null` to
// `IS NOT NULL`. Works on every column type (string, bool, integer,
// timestamp) without special casing.
func TestFilterToCondition_NullLiteral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		filter string
		want   string
	}{
		{filter: "name = null", want: "providers.name IS NULL"},
		{filter: "name != null", want: "providers.name IS NOT NULL"},
		{filter: "enabled = null", want: "providers.enabled IS NULL"},
		{filter: "priority != null", want: "providers.priority IS NOT NULL"},
		{filter: "created_at = null", want: "providers.created_at IS NULL"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.filter, func(t *testing.T) {
			t.Parallel()
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			assert.Contains(t, sqlFor(t, cond), tt.want)
		})
	}
}

// TestFilterToCondition_NullLiteralRejectsOrderingOps guards against
// silent empty-result sets — SQL returns UNKNOWN for `col < NULL`, the
// WHERE clause drops the row, the caller sees nothing. Reject at parse
// time so the error is visible.
func TestFilterToCondition_NullLiteralRejectsOrderingOps(t *testing.T) {
	t.Parallel()
	for _, f := range []string{
		"priority > null",
		"priority < null",
		"priority >= null",
		"priority <= null",
	} {
		f := f
		t.Run(f, func(t *testing.T) {
			t.Parallel()
			_, err := aipjet.FilterToCondition(f, fields())
			require.Error(t, err)
			assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
		})
	}
}

// TestFilterToCondition_DottedPath covers JSONB accessor binding —
// a caller-registered dotted key ("config.api_key_ref") compiles to
// `col->>'path'` and supports the same comparators as a bare string
// column.
func TestFilterToCondition_DottedPath(t *testing.T) {
	t.Parallel()
	fieldsWithPaths := aipjet.FilterFields{
		"name":                nameCol,
		"config.api_key_ref":  postgres.RawString("config->>'api_key_ref'"),
		"config.region.code":  postgres.RawString("config->'region'->>'code'"),
		"priority":            priorityCol,
		"metrics.error_count": postgres.RawString("(metrics->>'error_count')::integer"),
	}
	tests := []struct {
		filter string
		want   string
	}{
		{
			filter: `config.api_key_ref = "abc123"`,
			want:   "(config->>'api_key_ref') = 'abc123'",
		},
		{
			filter: `config.api_key_ref != "secret"`,
			want:   "(config->>'api_key_ref') != 'secret'",
		},
		{
			filter: `config.api_key_ref : "k-"`,
			want:   `LOWER(config->>'api_key_ref') LIKE '%k-%'`,
		},
		{
			filter: `config.region.code = "eu-west-1"`,
			want:   "(config->'region'->>'code') = 'eu-west-1'",
		},
		{
			filter: `config.api_key_ref = null`,
			want:   "(config->>'api_key_ref') IS NULL",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.filter, func(t *testing.T) {
			t.Parallel()
			cond, err := aipjet.FilterToCondition(tt.filter, fieldsWithPaths)
			require.NoError(t, err)
			assert.Contains(t, sqlFor(t, cond), tt.want)
		})
	}
}

// TestFilterToCondition_DottedPathUnknown pins that unregistered
// dotted paths are rejected cleanly. Silent acceptance would let
// callers query arbitrary JSONB subfields — an injection foot-gun
// and a seqscan trap.
func TestFilterToCondition_DottedPathUnknown(t *testing.T) {
	t.Parallel()
	// "config" is not a registered filter field at all.
	_, err := aipjet.FilterToCondition(`config.secret = "x"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
	assert.Contains(t, err.Error(), "config.secret")
}

// TestFilterToCondition_DottedPathInComposite confirms dotted paths
// compose with AND/OR and plain columns.
func TestFilterToCondition_DottedPathInComposite(t *testing.T) {
	t.Parallel()
	fieldsWithPaths := aipjet.FilterFields{
		"enabled":            enabledCol,
		"config.api_key_ref": postgres.RawString("config->>'api_key_ref'"),
	}
	cond, err := aipjet.FilterToCondition(
		`enabled = true AND config.api_key_ref = "live-key"`,
		fieldsWithPaths,
	)
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "enabled = TRUE")
	assert.Contains(t, sql, "(config->>'api_key_ref') = 'live-key'")
	assert.Contains(t, sql, " AND ")
}

// TestFilterToCondition_NullLiteralInComposite confirms the NULL
// predicate composes with AND/OR.
func TestFilterToCondition_NullLiteralInComposite(t *testing.T) {
	t.Parallel()
	cond, err := aipjet.FilterToCondition(`name != null AND enabled = true`, fields())
	require.NoError(t, err)
	sql := sqlFor(t, cond)
	assert.Contains(t, sql, "providers.name IS NOT NULL")
	assert.Contains(t, sql, "providers.enabled = TRUE")
	assert.Contains(t, sql, " AND ")
}

// TestFilterToCondition_FloatColumn — REAL / DOUBLE PRECISION columns
// take numeric literals on either side of the grammar's binary
// comparators. Ints coerce to float so `score > 42` works even when
// the literal has no decimal point — the parser has no way to know
// the column is float.
func TestFilterToCondition_FloatColumn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		filter string
		want   string
	}{
		{filter: `score = 3.14`, want: "providers.score = 3.14"},
		{filter: `score != 0.0`, want: "providers.score != 0"},
		{filter: `score > 42`, want: "providers.score > 42"},
		{filter: `score >= 0.1`, want: "providers.score >= 0.1"},
		{filter: `score < 1000000.0`, want: "providers.score < 1000000"},
		{filter: `score <= -2.5`, want: "providers.score <= -2.5"},
		{filter: `score = null`, want: "providers.score IS NULL"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.filter, func(t *testing.T) {
			t.Parallel()
			cond, err := aipjet.FilterToCondition(tt.filter, fields())
			require.NoError(t, err)
			assert.Contains(t, sqlFor(t, cond), tt.want)
		})
	}
}

// TestFilterToCondition_FloatColumnRejectsHas pins that `:` is not
// defined on float columns — substring match has no meaning there,
// and the parser's attempt would route a string literal into a
// float comparator.
func TestFilterToCondition_FloatColumnRejectsHas(t *testing.T) {
	t.Parallel()
	_, err := aipjet.FilterToCondition(`score : "3"`, fields())
	require.Error(t, err)
	assert.ErrorIs(t, err, aipjet.ErrUnsupportedFilter)
}
