package aip

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStringCodec_RoundTrip(t *testing.T) {
	codec := StringCodec{}
	encoded, err := codec.Encode("hello")
	require.NoError(t, err)
	assert.Equal(t, "hello", encoded)

	decoded, err := codec.Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, "hello", decoded)
}

func TestStringCodec_TypeMismatch(t *testing.T) {
	codec := StringCodec{}
	_, err := codec.Encode(42)
	assert.Error(t, err)
}

func TestBoolCodec_RoundTrip(t *testing.T) {
	codec := BoolCodec{}
	for _, val := range []bool{true, false} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)

		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, val, decoded)
	}
}

func TestInt64Codec_RoundTrip(t *testing.T) {
	codec := Int64Codec{}
	for _, val := range []int64{0, -1, 42, 9223372036854775807, -9223372036854775808} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)

		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, val, decoded)
	}
}

// TestInt64Codec_AcceptsInt32 pins that int32 cursor values (which is
// what jet models produce for INTEGER columns) round-trip through the
// codec and decode back as int64 — the keyset comparator expects one
// integer type to dispatch on.
func TestInt64Codec_AcceptsInt32(t *testing.T) {
	codec := Int64Codec{}
	for _, val := range []int32{0, -1, 42, 2147483647, -2147483648} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)

		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, int64(val), decoded)
	}
}

func TestFloat64Codec_RoundTrip(t *testing.T) {
	codec := Float64Codec{}
	for _, val := range []float64{
		0,
		-0,
		1,
		-1,
		3.141592653589793,
		1.7976931348623157e308,  // near MaxFloat64
		-1.7976931348623157e308, // near -MaxFloat64
		5e-324,                  // smallest positive subnormal
	} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)
		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, val, decoded, "round trip failed for %v", val)
	}
}

// TestFloat64Codec_AcceptsFloat32 mirrors the int32-is-accepted rule:
// REAL columns surface as float32 in the jet model but the keyset
// comparator wants float64, so the codec promotes at encode time.
func TestFloat64Codec_AcceptsFloat32(t *testing.T) {
	codec := Float64Codec{}
	for _, val := range []float32{0, -1, 3.14, 1e38, -1e38} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)
		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, float64(val), decoded, "round trip failed for %v", val)
	}
}

// TestFloat64Codec_RejectsNaN — NaN compares unequal to itself, so
// a cursor anchored at NaN would never match when pagination resumes.
// The codec refuses at encode time rather than issuing a page token
// that silently loops back to page 1.
func TestFloat64Codec_RejectsNaN(t *testing.T) {
	codec := Float64Codec{}
	nan := math.NaN()
	_, err := codec.Encode(nan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NaN")
}

// TestFloat64Codec_Infinity pins that ±Inf round-trips cleanly —
// valid IEEE-754 values, totally ordered against every finite float,
// so pagination cursors anchored at Inf work.
func TestFloat64Codec_Infinity(t *testing.T) {
	codec := Float64Codec{}
	for _, val := range []float64{math.Inf(1), math.Inf(-1)} {
		encoded, err := codec.Encode(val)
		require.NoError(t, err)
		decoded, err := codec.Decode(encoded)
		require.NoError(t, err)
		assert.Equal(t, val, decoded)
	}
}

func TestFloat64Codec_TypeMismatch(t *testing.T) {
	codec := Float64Codec{}
	_, err := codec.Encode("3.14")
	assert.Error(t, err)
}

func TestTimestampCodec_RoundTrip(t *testing.T) {
	codec := TimestampCodec{}
	now := time.Now().UTC()

	encoded, err := codec.Encode(now)
	require.NoError(t, err)

	decoded, err := codec.Decode(encoded)
	require.NoError(t, err)

	// Compare with nanosecond precision after UTC normalization.
	assert.True(t, now.Equal(decoded.(time.Time)))
}

func TestTimestampCodec_UTCNormalization(t *testing.T) {
	codec := TimestampCodec{}
	loc := time.FixedZone("UTC+5", 5*60*60)
	localTime := time.Date(2025, 1, 1, 12, 0, 0, 0, loc)

	encoded, err := codec.Encode(localTime)
	require.NoError(t, err)

	decoded, err := codec.Decode(encoded)
	require.NoError(t, err)

	decodedTime, ok := decoded.(time.Time)
	require.True(t, ok)
	assert.Equal(t, time.UTC, decodedTime.Location())
	assert.True(t, localTime.Equal(decodedTime))
}
