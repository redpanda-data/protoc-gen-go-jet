package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// validateOrderable gates cursor pagination: a column whose kind has no
// cursor codec would produce silently-wrong ordering (e.g. JSONB bytes
// compared lexically). The generator refuses at emit time so the
// broken schema never ships. These tests pin which kinds are allowed.
func TestValidateOrderable_KindsWithCodecs(t *testing.T) {
	t.Parallel()
	ok := []ColumnPlan{
		{Kind: KindTimestamp, JetGoType: "time.Time"},
		{Kind: KindDuration, JetGoType: "int64"},
		{Kind: KindEnumAsText, JetGoType: "string"},
		{Kind: KindScalar, JetGoType: "string"},
		{Kind: KindScalar, JetGoType: "int32"},
		{Kind: KindScalar, JetGoType: "int64"},
		{Kind: KindScalar, JetGoType: "float32"},
		{Kind: KindScalar, JetGoType: "float64"},
	}
	for _, c := range ok {
		assert.NoErrorf(t, validateOrderable(c), "kind=%d type=%q must be orderable", c.Kind, c.JetGoType)
	}
}

// TestValidateOrderable_RejectsBool pins that bool scalars are refused
// even though they're otherwise first-class scalar columns. Jet's
// BoolExpression has no GT/LT, and bool pagination is noise — the
// generator catches it here rather than emitting code that won't
// compile or a cursor fallback that can't skip past a bool value.
func TestValidateOrderable_RejectsBool(t *testing.T) {
	t.Parallel()
	err := validateOrderable(ColumnPlan{Kind: KindScalar, JetGoType: "bool"})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "bool")
	}
}

// TestValidateOrderable_RejectsNullable pins that nullable columns
// can't be orderable. The jet model pointerises them, the cursor
// codec expects a value, and the keyset comparator has no NULL
// partition handling — all three would need to change before this
// combination could work.
func TestValidateOrderable_RejectsNullable(t *testing.T) {
	t.Parallel()
	for _, c := range []ColumnPlan{
		{Kind: KindScalar, JetGoType: "string", Nullable: true},
		{Kind: KindTimestamp, JetGoType: "time.Time", Nullable: true},
		{Kind: KindScalar, JetGoType: "int64", Nullable: true},
	} {
		err := validateOrderable(c)
		if assert.Errorf(t, err, "kind=%d type=%q nullable must be rejected", c.Kind, c.JetGoType) {
			assert.Contains(t, err.Error(), "nullable")
		}
	}
}

func TestValidateOrderable_RejectsWithoutCodec(t *testing.T) {
	t.Parallel()
	// Scalar with a Go type that has no cursor codec (e.g. []byte for
	// BYTEA) must fail — comparing raw bytes across arbitrary encodings
	// doesn't give stable order.
	err := validateOrderable(ColumnPlan{Kind: KindScalar, JetGoType: "[]byte"})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), `JetGoType "[]byte"`)
	}

	// JSONB proto (nested message → JSONB). No codec: JSONB order
	// would be lexical-on-serialised-bytes, useless for pagination.
	err = validateOrderable(ColumnPlan{Kind: KindJSONBProto, JetGoType: "string"})
	assert.Error(t, err, "JSONBProto must not be orderable")

	// TEXT[] repeated. No codec: array order isn't a stable cursor.
	err = validateOrderable(ColumnPlan{Kind: KindRepeatedText, JetGoType: "pq.StringArray"})
	assert.Error(t, err, "repeated text must not be orderable")

	// JSONB string map. Same reason.
	err = validateOrderable(ColumnPlan{Kind: KindJSONBStrMap, JetGoType: "string"})
	assert.Error(t, err, "JSONB strmap must not be orderable")
}

// TestValidateOrderable_MessageMentionsSupportedKinds keeps the error
// message at the "fail pointing at the next step" level — when a
// developer flips `orderable: true` on a JSONB field the message
// should steer them away, not just fail abstractly.
func TestValidateOrderable_MessageMentionsSupportedKinds(t *testing.T) {
	t.Parallel()
	err := validateOrderable(ColumnPlan{Kind: KindJSONBProto})
	if assert.Error(t, err) {
		msg := err.Error()
		// The message names the gate: scalar / timestamp / duration / enum.
		// If the set changes, update both the impl and this test together.
		assert.True(t,
			strings.Contains(msg, "scalar") && strings.Contains(msg, "timestamp"),
			"error should name the orderable kinds: %s", msg)
	}
}

// TestCodecNameForColumn pins the ColumnKind + JetGoType → aip codec
// decision table. The keyset cursor serialises through the chosen
// codec; a wrong mapping produces unpages (e.g. a float column
// paged through StringCodec would lexicographically compare
// "1000" < "2" < "9") — subtle bugs that only surface on the
// second page of a list query.
func TestCodecNameForColumn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		col  ColumnPlan
		want string
	}{
		{"timestamp", ColumnPlan{Kind: KindTimestamp}, "TimestampCodec"},
		{"duration", ColumnPlan{Kind: KindDuration}, "Int64Codec"},
		{"string scalar", ColumnPlan{Kind: KindScalar, JetGoType: "string"}, "StringCodec"},
		{"int32 scalar", ColumnPlan{Kind: KindScalar, JetGoType: "int32"}, "Int64Codec"},
		{"int64 scalar", ColumnPlan{Kind: KindScalar, JetGoType: "int64"}, "Int64Codec"},
		{"float32 scalar", ColumnPlan{Kind: KindScalar, JetGoType: "float32"}, "Float64Codec"},
		{"float64 scalar", ColumnPlan{Kind: KindScalar, JetGoType: "float64"}, "Float64Codec"},
		// Enum stores as TEXT; the cursor serialises the enum name.
		{"enum", ColumnPlan{Kind: KindEnumAsText, JetGoType: "string"}, "StringCodec"},
		// Bytes scalars fall through to the default string codec — but
		// the validateOrderable gate rejects them before they get here.
		// Pin the default path anyway so we don't rely on the
		// validator alone.
		{"bytes fallback", ColumnPlan{Kind: KindScalar, JetGoType: "[]byte"}, "StringCodec"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, codecNameForColumn(tt.col))
		})
	}
}
