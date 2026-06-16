package jettypes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTimestampArray_Scan covers the PG array literal shapes pq
// surfaces. The Scan path has to tolerate the few timezone-offset
// spellings PG emits across versions; the fixtures below hit each
// layout in the parser's list.
func TestTimestampArray_Scan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want []time.Time
	}{
		{
			name: "empty",
			in:   []byte("{}"),
			want: []time.Time{},
		},
		{
			name: "single microsecond precision",
			in:   []byte(`{"2025-04-17 10:20:30.123456+00"}`),
			want: []time.Time{time.Date(2025, 4, 17, 10, 20, 30, 123456000, time.UTC)},
		},
		{
			name: "multiple mixed precision",
			in:   []byte(`{"2020-01-01 00:00:00.000001+00","2099-12-31 23:59:59+00","1970-01-01 00:00:00+00"}`),
			want: []time.Time{
				time.Date(2020, 1, 1, 0, 0, 0, 1000, time.UTC),
				time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
				time.Unix(0, 0).UTC(),
			},
		},
		{
			name: "colon-separated offset",
			in:   []byte(`{"2025-04-17 10:20:30+00:00"}`),
			want: []time.Time{time.Date(2025, 4, 17, 10, 20, 30, 0, time.UTC)},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got TimestampArray
			require.NoError(t, got.Scan(tt.in))
			require.Len(t, got, len(tt.want))
			for i, exp := range tt.want {
				assert.Truef(t, exp.Equal(got[i]), "element %d: want %v, got %v", i, exp, got[i])
				assert.Equal(t, time.UTC, got[i].Location(),
					"element %d must normalise to UTC", i)
			}
		})
	}
}

// TestTimestampArray_ScanNil — NULL column scans to a nil receiver
// slice, not an empty one. Matches pq.StringArray's behaviour.
func TestTimestampArray_ScanNil(t *testing.T) {
	t.Parallel()
	got := TimestampArray{time.Now()} // start non-nil
	require.NoError(t, got.Scan(nil))
	assert.Nil(t, got)
}

// TestTimestampArray_Value nil encodes as the empty PG array literal
// `{}`, not SQL NULL. Matches the plugin's other array kinds, whose
// columns default NOT NULL with `DEFAULT '{}'`.
func TestTimestampArray_ValueNil(t *testing.T) {
	t.Parallel()
	var a TimestampArray
	v, err := a.Value()
	require.NoError(t, err)
	// pq.Array returns driver.Value as string for arrays; accept
	// either shape the driver could hand back to keep the test
	// robust if pq's internal representation changes.
	switch s := v.(type) {
	case string:
		assert.Equal(t, "{}", s)
	case []byte:
		assert.Equal(t, "{}", string(s))
	default:
		t.Fatalf("unexpected driver.Value shape %T: %v", v, v)
	}
}

// TestTimestampArray_RoundTripShape encodes then decodes without a
// real driver. Proves the two halves agree on the wire format pq
// uses internally — if a future pq bump changes the Value layout,
// this catches the incompatibility without needing a testcontainer.
func TestTimestampArray_RoundTripShape(t *testing.T) {
	t.Parallel()
	orig := TimestampArray{
		time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 123456000, time.UTC),
	}
	encoded, err := orig.Value()
	require.NoError(t, err)

	var decoded TimestampArray
	require.NoError(t, decoded.Scan(encoded))
	require.Len(t, decoded, len(orig))
	for i, exp := range orig {
		assert.Truef(t, exp.Equal(decoded[i]), "element %d: want %v, got %v", i, exp, decoded[i])
	}
}
