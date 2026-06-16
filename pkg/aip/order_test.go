package aip

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOrderBy_Empty(t *testing.T) {
	ob, err := ParseOrderBy("")
	require.NoError(t, err)
	assert.Empty(t, ob.Fields)
}

func TestParseOrderBy_Whitespace(t *testing.T) {
	ob, err := ParseOrderBy("   ")
	require.NoError(t, err)
	assert.Empty(t, ob.Fields)
}

func TestParseOrderBy_SingleFieldDefaultAsc(t *testing.T) {
	ob, err := ParseOrderBy("display_name")
	require.NoError(t, err)
	require.Len(t, ob.Fields, 1)
	assert.Equal(t, "display_name", ob.Fields[0].Path)
	assert.Equal(t, Asc, ob.Fields[0].Direction)
}

func TestParseOrderBy_SingleFieldExplicitAsc(t *testing.T) {
	ob, err := ParseOrderBy("display_name asc")
	require.NoError(t, err)
	require.Len(t, ob.Fields, 1)
	assert.Equal(t, Asc, ob.Fields[0].Direction)
}

func TestParseOrderBy_SingleFieldDesc(t *testing.T) {
	ob, err := ParseOrderBy("created_at desc")
	require.NoError(t, err)
	require.Len(t, ob.Fields, 1)
	assert.Equal(t, "created_at", ob.Fields[0].Path)
	assert.Equal(t, Desc, ob.Fields[0].Direction)
}

func TestParseOrderBy_MultipleFields(t *testing.T) {
	ob, err := ParseOrderBy("display_name asc, created_at desc")
	require.NoError(t, err)
	require.Len(t, ob.Fields, 2)
	assert.Equal(t, "display_name", ob.Fields[0].Path)
	assert.Equal(t, Asc, ob.Fields[0].Direction)
	assert.Equal(t, "created_at", ob.Fields[1].Path)
	assert.Equal(t, Desc, ob.Fields[1].Direction)
}

func TestParseOrderBy_NestedFieldPath(t *testing.T) {
	ob, err := ParseOrderBy("labels.environment desc")
	require.NoError(t, err)
	require.Len(t, ob.Fields, 1)
	assert.Equal(t, "labels.environment", ob.Fields[0].Path)
}

func TestParseOrderBy_CaseInsensitiveDirection(t *testing.T) {
	ob, err := ParseOrderBy("name DESC")
	require.NoError(t, err)
	assert.Equal(t, Desc, ob.Fields[0].Direction)

	ob, err = ParseOrderBy("name AsC")
	require.NoError(t, err)
	assert.Equal(t, Asc, ob.Fields[0].Direction)
}

func TestParseOrderBy_InvalidCharacter(t *testing.T) {
	_, err := ParseOrderBy("name; DROP TABLE")
	assert.ErrorIs(t, err, ErrInvalidOrderBy)
}

func TestParseOrderBy_DuplicateField(t *testing.T) {
	_, err := ParseOrderBy("name asc, name desc")
	assert.ErrorIs(t, err, ErrInvalidOrderBy)
}

func TestParseOrderBy_InvalidDirection(t *testing.T) {
	_, err := ParseOrderBy("name sideways")
	assert.ErrorIs(t, err, ErrInvalidOrderBy)
}

func TestParseOrderBy_TooManyParts(t *testing.T) {
	_, err := ParseOrderBy("name asc extra")
	assert.ErrorIs(t, err, ErrInvalidOrderBy)
}

func TestParseOrderBy_EmptyFieldSpec(t *testing.T) {
	_, err := ParseOrderBy("name,")
	assert.ErrorIs(t, err, ErrInvalidOrderBy)
}

func TestIsUniformDirection(t *testing.T) {
	assert.True(t, IsUniformDirection(OrderBy{}))
	assert.True(t, IsUniformDirection(OrderBy{Fields: []OrderField{{Direction: Asc}}}))
	assert.True(t, IsUniformDirection(OrderBy{Fields: []OrderField{{Direction: Asc}, {Direction: Asc}}}))
	assert.True(t, IsUniformDirection(OrderBy{Fields: []OrderField{{Direction: Desc}, {Direction: Desc}}}))
	assert.False(t, IsUniformDirection(OrderBy{Fields: []OrderField{{Direction: Asc}, {Direction: Desc}}}))
}

func TestAppendUniqueFields(t *testing.T) {
	dst := []OrderField{{Path: "a", Direction: Asc}}
	result := AppendUniqueFields(dst, OrderField{Path: "a", Direction: Desc}, OrderField{Path: "b", Direction: Asc})
	assert.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Path)
	assert.Equal(t, Asc, result[0].Direction) // original preserved
	assert.Equal(t, "b", result[1].Path)
}
