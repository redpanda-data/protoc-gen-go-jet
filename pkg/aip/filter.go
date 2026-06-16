package aip

import (
	"fmt"
	"strings"
)

// MakeNameFilter returns an AIP-160 filter expression for substring
// matching on the `name` field. Empty input returns empty output so the
// result can flow straight into Params.Filter unchanged.
//
// MakeNameFilter and ParseNameFilter are round-trip inverses: any
// non-empty string this function returns is guaranteed to parse back
// into the same needle. If nameContains contains characters the parser
// can't handle (embedded `"` or `\`), the function returns empty to
// stop the service from silently producing an unparseable filter.
// Name values today are constrained to `[a-z][a-z0-9-]*` so this can't
// fire in the current code paths, but the guard keeps the invariant
// honest as the filter surface grows.
//
// This exists so service layers that accept a structured filter
// (`name_contains`) in their proto request have one obvious encoding
// into the AIP-160 string that repositories parse.
func MakeNameFilter(nameContains string) string {
	if nameContains == "" || strings.ContainsAny(nameContains, `"\`) {
		return ""
	}
	return `name:"` + nameContains + `"`
}

// ParseNameFilter is the inverse of MakeNameFilter. An empty input
// returns an empty name with no error (meaning "no filter"). Any
// unrecognised filter string is rejected — better to fail the request
// than silently return unfiltered rows when a filter was requested.
//
// This is the minimal AIP-160 subset needed by the current resource
// services (one field, one operator, one quoted string literal). When
// callers need more, this gets replaced by a real AIP-160 parser and
// the return type becomes a structured filter AST.
func ParseNameFilter(filter string) (string, error) {
	if filter == "" {
		return "", nil
	}
	const prefix = `name:"`
	if !strings.HasPrefix(filter, prefix) || !strings.HasSuffix(filter, `"`) {
		return "", fmt.Errorf("unsupported filter %q (want name:%q)", filter, "VALUE")
	}
	body := filter[len(prefix) : len(filter)-1]
	// Reject embedded unescaped quotes — no escape handling today, so a
	// raw quote means the caller sent something we can't round-trip.
	if strings.Contains(body, `"`) {
		return "", fmt.Errorf("invalid filter %q: unescaped quote inside value", filter)
	}
	return body, nil
}
