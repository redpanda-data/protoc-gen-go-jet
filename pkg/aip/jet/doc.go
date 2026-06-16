// Package jet provides go-jet backed execution of AIP-compliant paginated
// list queries, and a translator from AIP-160 filter expressions into
// go-jet BoolExpressions.
//
// Three things live in this package:
//
//   - Schema, NewSchema, Fields — describe a resource's orderable columns
//     and their cursor codecs so the keyset pagination engine can emit
//     stable cursors.
//   - Execute, ExecuteWithCondition — run a paginated list query and
//     return rows + next page token. Pass the result of
//     FilterToCondition as the baseCondition to combine the filter with
//     the cursor predicate.
//   - FilterToCondition, FilterFields — parse an AIP-160 filter string
//     into a jet BoolExpression. Supports =, !=, <, <=, >, >=, :,
//     NOT/-, AND, OR, parens, timestamp("...") and duration("...")
//     literals. Typed per column, with security guards (LIKE metachar
//     escaping, 4 KiB input cap).
//
// Both halves combined give a drop-in list-with-filter implementation:
//
//	cond, err := jet.FilterToCondition(params.Filter, jet.FilterFields{...})
//	if err != nil { return ... }
//	rows, next, err := jet.ExecuteWithCondition(ctx, schema, params, query, cond, db)
//
// See [aip] for the backend-neutral types and helpers.
package jet
