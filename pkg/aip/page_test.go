package aip

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeToken_RoundTrip(t *testing.T) {
	codecs := map[string]CursorCodec{
		"name":       StringCodec{},
		"created_at": TimestampCodec{},
	}
	order := OrderBy{Fields: []OrderField{
		{Path: "name", Direction: Asc},
		{Path: "created_at", Direction: Desc},
	}}
	now := time.Now().UTC().Truncate(time.Nanosecond)

	encoded, err := EncodeToken("test/Resource", []any{"alice", now}, order, "", codecs)
	require.NoError(t, err)
	assert.NotEmpty(t, encoded)

	token, err := DecodeToken(encoded)
	require.NoError(t, err)
	require.NotNil(t, token)

	assert.Equal(t, "test/Resource", token.ResourceType)
	assert.Equal(t, "", token.FilterHash)
	require.Len(t, token.CursorValues, 2)
	assert.Equal(t, "alice", token.CursorValues[0])
	require.Len(t, token.OrderFields, 2)
	assert.Equal(t, "name", token.OrderFields[0].Field)
	assert.False(t, token.OrderFields[0].Desc)
	assert.Equal(t, "created_at", token.OrderFields[1].Field)
	assert.True(t, token.OrderFields[1].Desc)
}

func TestDecodeToken_Empty(t *testing.T) {
	token, err := DecodeToken("")
	require.NoError(t, err)
	assert.Nil(t, token)
}

func TestDecodeToken_InvalidBase64(t *testing.T) {
	_, err := DecodeToken("not-valid-base64!!!")
	assert.Error(t, err)
}

func TestDecodeToken_InvalidJSON(t *testing.T) {
	// Valid base64 but not valid JSON
	_, err := DecodeToken("bm90LWpzb24") // "not-json" in base64
	assert.Error(t, err)
}

func TestValidateToken_Nil(t *testing.T) {
	err := ValidateToken(nil, "test/Resource", "")
	assert.NoError(t, err)
}

func TestValidateToken_Expired(t *testing.T) {
	token := &PageToken{
		CreateTime:   time.Now().Add(-25 * time.Hour).UnixNano(),
		ResourceType: "test/Resource",
	}
	err := ValidateToken(token, "test/Resource", "")
	assert.ErrorIs(t, err, errTokenExpired)
}

func TestValidateToken_WrongResourceType(t *testing.T) {
	token := &PageToken{
		CreateTime:   time.Now().UnixNano(),
		ResourceType: "test/Other",
	}
	err := ValidateToken(token, "test/Resource", "")
	assert.ErrorIs(t, err, errTokenWrongResource)
}

func TestValidateToken_FilterMismatch(t *testing.T) {
	token := &PageToken{
		CreateTime:   time.Now().UnixNano(),
		ResourceType: "test/Resource",
		FilterHash:   hashFilter("filter1"),
	}
	err := ValidateToken(token, "test/Resource", "filter2")
	assert.ErrorIs(t, err, ErrFilterMismatch)
}

func TestValidateToken_FilterMatch(t *testing.T) {
	token := &PageToken{
		CreateTime:   time.Now().UnixNano(),
		ResourceType: "test/Resource",
		FilterHash:   hashFilter("same-filter"),
	}
	err := ValidateToken(token, "test/Resource", "same-filter")
	assert.NoError(t, err)
}

func TestHashFilter_EmptyIsEmpty(t *testing.T) {
	assert.Equal(t, "", hashFilter(""))
}

func TestHashFilter_Deterministic(t *testing.T) {
	h1 := hashFilter("test")
	h2 := hashFilter("test")
	assert.Equal(t, h1, h2)
	assert.NotEmpty(t, h1)
}

func TestHashFilter_DifferentInputsDifferentHashes(t *testing.T) {
	assert.NotEqual(t, hashFilter("a"), hashFilter("b"))
}
