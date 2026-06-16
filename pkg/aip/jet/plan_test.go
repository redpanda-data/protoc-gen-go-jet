package jet

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

func TestBuildPlan_DefaultPageSize(t *testing.T) {
	s := newTestSchema()
	plan, err := BuildPlan(s, aip.Params{})
	require.NoError(t, err)
	assert.Equal(t, int32(50), plan.PageSize)
}

func TestBuildPlan_ZeroPageSizeUsesDefault(t *testing.T) {
	s := newTestSchema()
	plan, err := BuildPlan(s, aip.Params{PageSize: 0})
	require.NoError(t, err)
	assert.Equal(t, int32(50), plan.PageSize)
}

func TestBuildPlan_NegativePageSizeUsesDefault(t *testing.T) {
	s := newTestSchema()
	plan, err := BuildPlan(s, aip.Params{PageSize: -5})
	require.NoError(t, err)
	assert.Equal(t, int32(50), plan.PageSize)
}

func TestBuildPlan_ExceedsMaxIsClamped(t *testing.T) {
	s := newTestSchema(WithMaxPageSize(100))
	plan, err := BuildPlan(s, aip.Params{PageSize: 200})
	require.NoError(t, err)
	assert.Equal(t, int32(100), plan.PageSize)
}

func TestBuildPlan_ValidPageSize(t *testing.T) {
	s := newTestSchema()
	plan, err := BuildPlan(s, aip.Params{PageSize: 25})
	require.NoError(t, err)
	assert.Equal(t, int32(25), plan.PageSize)
}

func TestBuildPlan_InvalidOrderBy(t *testing.T) {
	s := newTestSchema()
	_, err := BuildPlan(s, aip.Params{OrderBy: "nonexistent"})
	assert.ErrorIs(t, err, aip.ErrInvalidOrderBy)
}

func TestBuildPlan_InvalidPageToken(t *testing.T) {
	s := newTestSchema()
	_, err := BuildPlan(s, aip.Params{PageToken: "invalid-token"})
	assert.ErrorIs(t, err, aip.ErrInvalidPageToken)
}

func TestBuildPlan_FilterMismatch(t *testing.T) {
	s := newTestSchema()

	// Create a token with one filter.
	codecs := map[string]aip.CursorCodec{
		"created_at": aip.TimestampCodec{},
		"id":         aip.StringCodec{},
	}
	order := aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Asc},
	}}
	token, err := aip.EncodeToken("test/TestModel", []any{time.Now().UTC(), "abc"}, order, "filter-a", codecs)
	require.NoError(t, err)

	// Use a different filter.
	_, err = BuildPlan(s, aip.Params{PageToken: token, Filter: "filter-b"})
	assert.ErrorIs(t, err, aip.ErrFilterMismatch)
}

func TestBuildPlan_ValidTokenDecodesCursor(t *testing.T) {
	s := newTestSchema()

	codecs := map[string]aip.CursorCodec{
		"created_at": aip.TimestampCodec{},
		"id":         aip.StringCodec{},
	}
	order := aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Asc},
	}}
	ts := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	token, err := aip.EncodeToken("test/TestModel", []any{ts, "abc"}, order, "", codecs)
	require.NoError(t, err)

	plan, err := BuildPlan(s, aip.Params{PageToken: token})
	require.NoError(t, err)

	require.Len(t, plan.CursorValues, 2)
	decodedTime, ok := plan.CursorValues[0].(time.Time)
	require.True(t, ok)
	assert.True(t, ts.Equal(decodedTime))
	assert.Equal(t, "abc", plan.CursorValues[1])
}
