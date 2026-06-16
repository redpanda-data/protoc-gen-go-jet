// Package aip provides AIP-compliant types and helpers for API resource
// management: keyset pagination types (AIP-158), order-by parsing,
// opaque page token encoding, and a minimal AIP-160 filter-string
// builder/parser (MakeNameFilter / ParseNameFilter) used by service-side
// encoders and memory-repo parsers that can't pull in go-jet.
//
// For go-jet backed PostgreSQL query execution and the full AIP-160
// expression → go-jet BoolExpression translator, see the [jet] subpackage.
package aip
