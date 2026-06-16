package aip

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// CursorCodec serializes and deserializes a single cursor field value for
// storage inside the opaque page token (see [EncodeToken], [DecodeToken]).
//
// Page tokens are base64-encoded JSON, which has no native time.Time and
// represents all numbers as float64. Codecs convert each Go value to a string
// on the way in and back to the exact original type on the way out so that
// downstream keyset comparisons (>, <, =) never see precision loss or type
// mismatches.
type CursorCodec interface {
	Encode(v any) (string, error)
	Decode(s string) (any, error)
}

// StringCodec serializes string cursor values.
type StringCodec struct{}

// Encode implements [CursorCodec].
func (StringCodec) Encode(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected string got %T", v)
	}
	return s, nil
}

// Decode implements [CursorCodec].
func (StringCodec) Decode(s string) (any, error) { return s, nil }

// BoolCodec serializes boolean cursor values.
type BoolCodec struct{}

// Encode implements [CursorCodec].
func (BoolCodec) Encode(v any) (string, error) {
	b, ok := v.(bool)
	if !ok {
		return "", fmt.Errorf("expected bool got %T", v)
	}
	return strconv.FormatBool(b), nil
}

// Decode implements [CursorCodec].
func (BoolCodec) Decode(s string) (any, error) {
	return strconv.ParseBool(s)
}

// Int64Codec serializes integer cursor values as strings to avoid precision loss.
//
// Accepts int32 as well as int64 — INTEGER columns surface in jet models
// as int32, and it's cleaner to absorb that at the codec than to force
// every caller to promote. Decode always returns int64 so the keyset
// comparator has one type to dispatch on.
type Int64Codec struct{}

// Encode implements [CursorCodec].
func (Int64Codec) Encode(v any) (string, error) {
	switch i := v.(type) {
	case int64:
		return strconv.FormatInt(i, 10), nil
	case int32:
		return strconv.FormatInt(int64(i), 10), nil
	default:
		return "", fmt.Errorf("expected int32 or int64 got %T", v)
	}
}

// Decode implements [CursorCodec].
func (Int64Codec) Decode(s string) (any, error) {
	return strconv.ParseInt(s, 10, 64)
}

// Float64Codec serializes floating-point cursor values as strconv's
// "shortest round-trippable" form — identical to the default fmt %v
// representation. Accepts float32 as well as float64 so REAL columns
// (surfaced as float32 in the jet model) work without caller coercion.
// Decode always returns float64 for a single keyset comparator type.
//
// Every finite IEEE-754 double survives the round-trip bit-exact via
// strconv's 'g' precision -1 path. NaN cursors are rejected at encode
// time because NaN compares unequal to itself — a cursor built on NaN
// would never match when paging resumes.
type Float64Codec struct{}

// Encode implements [CursorCodec].
func (Float64Codec) Encode(v any) (string, error) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	default:
		return "", fmt.Errorf("expected float32 or float64 got %T", v)
	}
	// NaN != NaN under IEEE-754, so resuming a cursor at NaN would
	// never find the next row. Reject at encode time rather than
	// issuing a page token that silently loops back to page 1.
	if math.IsNaN(f) {
		return "", errors.New("NaN is not a valid cursor value")
	}
	return strconv.FormatFloat(f, 'g', -1, 64), nil
}

// Decode implements [CursorCodec].
func (Float64Codec) Decode(s string) (any, error) {
	return strconv.ParseFloat(s, 64)
}

// TimestampCodec stores time.Time as RFC-3339-nano strings.
// Always converts to UTC to avoid timezone-dependent cursor comparisons.
type TimestampCodec struct{}

// Encode implements [CursorCodec].
func (TimestampCodec) Encode(v any) (string, error) {
	t, ok := v.(time.Time)
	if !ok {
		return "", fmt.Errorf("expected time.Time got %T", v)
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// Decode implements [CursorCodec].
func (TimestampCodec) Decode(s string) (any, error) {
	return time.Parse(time.RFC3339Nano, s)
}
