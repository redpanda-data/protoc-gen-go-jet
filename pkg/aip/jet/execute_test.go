package jet

import (
	"testing"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

func TestBuildJetKeysetCondition_EmptyCursor(t *testing.T) {
	order := aip.OrderBy{Fields: []aip.OrderField{{Path: "id", Direction: aip.Asc}}}
	cond, err := buildKeysetCondition(order, nil, Fields[testModel]{
		"id": {Column: testIDCol, Codec: aip.StringCodec{}},
	})
	require.NoError(t, err)
	assert.Nil(t, cond)
}

func TestBuildJetKeysetCondition_NoOrderFields(t *testing.T) {
	_, err := buildKeysetCondition(aip.OrderBy{}, []any{"val"}, Fields[testModel]{})
	assert.Error(t, err)
}

func TestBuildJetKeysetCondition_LengthMismatch(t *testing.T) {
	order := aip.OrderBy{Fields: []aip.OrderField{{Path: "id", Direction: aip.Asc}}}
	_, err := buildKeysetCondition(order, []any{"a", "b"}, Fields[testModel]{
		"id": {Column: testIDCol, Codec: aip.StringCodec{}},
	})
	assert.Error(t, err)
}

func TestBuildTupleComparison_Asc(t *testing.T) {
	order := aip.OrderBy{Fields: []aip.OrderField{
		{Path: "display_name", Direction: aip.Asc},
		{Path: "id", Direction: aip.Asc},
	}}
	fields := Fields[testModel]{
		"display_name": {Column: testDisplayNameCol, Codec: aip.StringCodec{}},
		"id":           {Column: testIDCol, Codec: aip.StringCodec{}},
	}

	cond, err := buildTupleComparison(order, []any{"alice", "123"}, fields)
	require.NoError(t, err)
	assert.NotNil(t, cond)
}

func TestBuildTupleComparison_Desc(t *testing.T) {
	order := aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Desc},
	}}
	fields := Fields[testModel]{
		"created_at": {Column: testCreatedAtCol, Codec: aip.TimestampCodec{}},
		"id":         {Column: testIDCol, Codec: aip.StringCodec{}},
	}

	cond, err := buildTupleComparison(order, []any{time.Now(), "abc"}, fields)
	require.NoError(t, err)
	assert.NotNil(t, cond)
}

func TestBuildLexicographicFallback_MixedDirection(t *testing.T) {
	order := aip.OrderBy{Fields: []aip.OrderField{
		{Path: "display_name", Direction: aip.Asc},
		{Path: "created_at", Direction: aip.Desc},
	}}
	fields := Fields[testModel]{
		"display_name": {Column: testDisplayNameCol, Codec: aip.StringCodec{}},
		"created_at":   {Column: testCreatedAtCol, Codec: aip.TimestampCodec{}},
	}

	cond, err := buildLexicographicFallback(order, []any{"alice", time.Now()}, fields)
	require.NoError(t, err)
	assert.NotNil(t, cond)
}

func TestCombineJetConditions_AllNil(t *testing.T) {
	assert.Nil(t, combineConditions(nil, nil))
}

func TestCombineJetConditions_OneNonNil(t *testing.T) {
	cond := testIDCol.(postgres.StringExpression).EQ(postgres.String("x"))
	result := combineConditions(nil, cond, nil)
	assert.NotNil(t, result)
}

func TestToLiteral_SupportedTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"string", "hello"},
		{"bool", true},
		{"int64", int64(42)},
		{"time", time.Now()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lit, err := toLiteral(tt.value)
			require.NoError(t, err)
			assert.NotNil(t, lit)
		})
	}
}

func TestToLiteral_UnsupportedType(t *testing.T) {
	// complex64 is not a supported cursor value type. 3.14 used to
	// be the sentinel here, but float64 is now accepted by toLiteral
	// for REAL / DOUBLE PRECISION columns.
	_, err := toLiteral(complex64(1 + 2i))
	assert.Error(t, err)
}

// TestToLiteral_Int32 pins that int32 cursor values — what jet models
// surface INTEGER columns as — are promoted to int64 literals and not
// rejected. Without this the first paginated List on any int32
// ordering column errors at runtime with "toLiteral not implemented
// for int32".
func TestToLiteral_Int32(t *testing.T) {
	lit, err := toLiteral(int32(42))
	require.NoError(t, err)
	assert.NotNil(t, lit)
}

// TestEqualityExpr_Int32 and TestDirectionExpr_Int32 cover the same
// int32 path for the lexicographic cursor fallback. If any cursor
// component is int32 and the ORDER BY is mixed-direction, the
// fallback fires and both helpers must accept int32.
func TestEqualityExpr_Int32(t *testing.T) {
	expr, err := equalityExpr(postgres.IntegerColumn("priority"), int32(42))
	require.NoError(t, err)
	assert.NotNil(t, expr)
}

func TestDirectionExpr_Int32(t *testing.T) {
	for _, dir := range []aip.SortDirection{aip.Asc, aip.Desc} {
		expr, err := directionExpr(postgres.IntegerColumn("priority"), dir, int32(42))
		require.NoError(t, err)
		assert.NotNil(t, expr)
	}
}

func TestBuildNextPageToken_NoMorePages(t *testing.T) {
	s := newTestSchema()
	plan := &aip.Plan{PageSize: 10, OrderBy: aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Asc},
	}}}

	// Fewer rows than page size => no next token.
	rows := make([]testModel, 5)
	token, err := buildNextPageToken(s, plan, rows)
	require.NoError(t, err)
	assert.Empty(t, token)
}

func TestBuildNextPageToken_ExactlyPageSize(t *testing.T) {
	s := newTestSchema()
	plan := &aip.Plan{PageSize: 5, OrderBy: aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Asc},
	}}}

	rows := make([]testModel, 5)
	token, err := buildNextPageToken(s, plan, rows)
	require.NoError(t, err)
	assert.Empty(t, token) // Exactly page size means no more pages.
}

func TestBuildNextPageToken_HasMorePages(t *testing.T) {
	s := newTestSchema()
	plan := &aip.Plan{PageSize: 2, OrderBy: aip.OrderBy{Fields: []aip.OrderField{
		{Path: "created_at", Direction: aip.Desc},
		{Path: "id", Direction: aip.Asc},
	}}}

	now := time.Now().UTC()
	rows := []testModel{
		{ID: "1", CreatedAt: now},
		{ID: "2", CreatedAt: now.Add(-time.Second)},
		{ID: "3", CreatedAt: now.Add(-2 * time.Second)}, // sentinel row
	}

	token, err := buildNextPageToken(s, plan, rows)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	// The cursor should be from rows[1] (last returned row, not the sentinel).
	decoded, err := aip.DecodeToken(token)
	require.NoError(t, err)
	assert.Equal(t, "2", decoded.CursorValues[1]) // id of the cursor row
}
