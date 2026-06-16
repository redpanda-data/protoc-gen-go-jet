package aip

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMakeParseNameFilter_Roundtrip is the invariant the two helpers
// sign up for: Make's output is either empty or something Parse can
// round-trip losslessly. Prior asymmetry (Make used %q, Parse didn't
// handle the `\"` escape) got flagged in reviewer round 8; the fix
// here is to make Make reject uninvertible inputs upfront.
func TestMakeParseNameFilter_Roundtrip(t *testing.T) {
	t.Parallel()
	for _, needle := range []string{
		"",
		"openai",
		"has-hyphen",
		"UPPER",
		"numbers123",
	} {
		filter := MakeNameFilter(needle)
		parsed, err := ParseNameFilter(filter)
		require.NoError(t, err, "needle %q produced %q which failed to parse", needle, filter)
		assert.Equal(t, needle, parsed, "round-trip for needle %q via filter %q", needle, filter)
	}
}

// TestMakeNameFilter_RejectsUninvertible pins that inputs the parser
// couldn't round-trip get dropped to empty. This is the fail-closed
// half of the invariant — a service that MakeNameFilter-ed an odd
// value produces "no filter" rather than an unparseable filter.
func TestMakeNameFilter_RejectsUninvertible(t *testing.T) {
	t.Parallel()
	for _, needle := range []string{
		`quo"te`,
		`back\slash`,
		`both"and\`,
	} {
		got := MakeNameFilter(needle)
		assert.Empty(t, got, "needle %q must fail to encode (contains meta chars)", needle)
	}
}

// TestParseNameFilter_EmptyIsNoError — empty filter means "no filter"
// not "invalid filter". Repo layer uses this to skip the WHERE clause.
func TestParseNameFilter_EmptyIsNoError(t *testing.T) {
	t.Parallel()
	v, err := ParseNameFilter("")
	require.NoError(t, err)
	assert.Empty(t, v)
}

// TestParseNameFilter_RejectsBadShapes keeps the rejection path pinned.
// If a new filter shape gets added later it will have to touch this
// test too, which is the right reminder.
func TestParseNameFilter_RejectsBadShapes(t *testing.T) {
	t.Parallel()
	for _, filter := range []string{
		`name=openai`,           // wrong operator
		`name:openai`,           // no quotes
		`display_name:"openai"`, // different field
		`name:"open"ai"`,        // embedded quote
		`"openai"`,              // no field
	} {
		_, err := ParseNameFilter(filter)
		require.Errorf(t, err, "filter %q must reject", filter)
	}
}
