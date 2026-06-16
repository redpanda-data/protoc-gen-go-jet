package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestIsUnsignedInt32Kind pins the proto3 kinds that protoc-gen-go
// renders as `uint32` in Go. The plugin stores these in `int32` jet
// columns, so the mapper relies on this predicate to emit explicit
// conversions — silently flipping a kind between signed/unsigned
// would produce either compile errors or a two's-complement round-
// trip bug.
func TestIsUnsignedInt32Kind(t *testing.T) {
	t.Parallel()
	unsigned := []protoreflect.Kind{protoreflect.Uint32Kind, protoreflect.Fixed32Kind}
	for _, k := range unsigned {
		assert.True(t, isUnsignedInt32Kind(k), "%v must be unsigned 32", k)
	}
	// Every other proto3 integer-ish kind must NOT match — especially
	// the signed 32-bit kinds that share the same SQL type.
	notUnsigned32 := []protoreflect.Kind{
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.StringKind, protoreflect.BoolKind, protoreflect.BytesKind,
		protoreflect.FloatKind, protoreflect.DoubleKind,
		protoreflect.EnumKind, protoreflect.MessageKind,
	}
	for _, k := range notUnsigned32 {
		assert.False(t, isUnsignedInt32Kind(k), "%v must not be unsigned 32", k)
	}
}

// TestIsUnsignedInt64Kind mirrors the 32-bit test for the 64-bit
// unsigned kinds. Shares the same rationale — the jet column stores
// `int64` so the mapper needs to cast deliberately.
func TestIsUnsignedInt64Kind(t *testing.T) {
	t.Parallel()
	unsigned := []protoreflect.Kind{protoreflect.Uint64Kind, protoreflect.Fixed64Kind}
	for _, k := range unsigned {
		assert.True(t, isUnsignedInt64Kind(k), "%v must be unsigned 64", k)
	}
	notUnsigned64 := []protoreflect.Kind{
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.StringKind, protoreflect.BoolKind, protoreflect.BytesKind,
		protoreflect.FloatKind, protoreflect.DoubleKind,
		protoreflect.EnumKind, protoreflect.MessageKind,
	}
	for _, k := range notUnsigned64 {
		assert.False(t, isUnsignedInt64Kind(k), "%v must not be unsigned 64", k)
	}
}

// TestGoMapKeyType pins the proto3-legal map key kinds to their
// protoc-gen-go Go types. The mapper uses this to emit the map
// literal `map[<Go>]<V>{}` — a wrong mapping breaks at compile.
// Any proto3-illegal kind returns "" so the caller can reject
// cleanly rather than hand a downstream template a garbage type.
func TestGoMapKeyType(t *testing.T) {
	t.Parallel()
	tests := map[protoreflect.Kind]string{
		protoreflect.StringKind:   "string",
		protoreflect.BoolKind:     "bool",
		protoreflect.Int32Kind:    "int32",
		protoreflect.Sint32Kind:   "int32",
		protoreflect.Sfixed32Kind: "int32",
		protoreflect.Uint32Kind:   "uint32",
		protoreflect.Fixed32Kind:  "uint32",
		protoreflect.Int64Kind:    "int64",
		protoreflect.Sint64Kind:   "int64",
		protoreflect.Sfixed64Kind: "int64",
		protoreflect.Uint64Kind:   "uint64",
		protoreflect.Fixed64Kind:  "uint64",
		// Proto3 map keys can't be float, bytes, enum, message; these
		// return "" so the caller can reject.
		protoreflect.FloatKind:   "",
		protoreflect.DoubleKind:  "",
		protoreflect.BytesKind:   "",
		protoreflect.EnumKind:    "",
		protoreflect.MessageKind: "",
	}
	for k, want := range tests {
		assert.Equal(t, want, goMapKeyType(k), "goMapKeyType(%v)", k)
	}
}

// TestGoMapScalarValueType pins the scalar value-kind → Go-type
// mapping used when a map<K, V> has a non-message V. Unlike the key
// test, every proto3 scalar kind is legal here — float/double/bytes
// included. Only proto-level "non-scalars" (enum stored as name,
// message as JSONB) return "" and route through a different
// code path.
func TestGoMapScalarValueType(t *testing.T) {
	t.Parallel()
	tests := map[protoreflect.Kind]string{
		protoreflect.StringKind:   "string",
		protoreflect.BoolKind:     "bool",
		protoreflect.BytesKind:    "[]byte",
		protoreflect.Int32Kind:    "int32",
		protoreflect.Sint32Kind:   "int32",
		protoreflect.Sfixed32Kind: "int32",
		protoreflect.Uint32Kind:   "uint32",
		protoreflect.Fixed32Kind:  "uint32",
		protoreflect.Int64Kind:    "int64",
		protoreflect.Sint64Kind:   "int64",
		protoreflect.Sfixed64Kind: "int64",
		protoreflect.Uint64Kind:   "uint64",
		protoreflect.Fixed64Kind:  "uint64",
		protoreflect.FloatKind:    "float32",
		protoreflect.DoubleKind:   "float64",
		// Non-scalar value kinds return "" — caller routes through
		// the enum-as-TEXT or JSONB-message paths instead.
		protoreflect.EnumKind:    "",
		protoreflect.MessageKind: "",
	}
	for k, want := range tests {
		assert.Equal(t, want, goMapScalarValueType(k), "goMapScalarValueType(%v)", k)
	}
}
