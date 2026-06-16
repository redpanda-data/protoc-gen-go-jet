package aip

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// TokenMaxAge is the maximum age of a page token before it expires.
const TokenMaxAge = 24 * time.Hour

// PageToken is the structure serialized into the opaque page token.
type PageToken struct {
	CreateTime   int64        `json:"ct"`           // unix nanos
	ResourceType string       `json:"rt"`           // resource type identifier
	FilterHash   string       `json:"fh,omitempty"` // SHA256 of filter string
	CursorValues []string     `json:"cv"`           // codec-encoded cursor values
	OrderFields  []TokenOrder `json:"of"`           // ordering used for this cursor
}

// TokenOrder represents a single order field inside a page token.
type TokenOrder struct {
	Field string `json:"f"`           // field path
	Desc  bool   `json:"d,omitempty"` // true if descending
}

// EncodeToken creates a new base64-encoded page token from cursor values and request parameters.
func EncodeToken(
	resourceType string,
	cursorValues []any,
	orderBy OrderBy,
	filter string,
	fields map[string]CursorCodec,
) (string, error) {
	if len(cursorValues) != len(orderBy.Fields) {
		return "", fmt.Errorf("cursor values count (%d) does not match order fields count (%d)",
			len(cursorValues), len(orderBy.Fields))
	}

	encoded := make([]string, len(cursorValues))
	for i, value := range cursorValues {
		fieldPath := orderBy.Fields[i].Path

		codec, ok := fields[fieldPath]
		if !ok {
			return "", fmt.Errorf("no codec registered for field %q", fieldPath)
		}

		s, err := codec.Encode(value)
		if err != nil {
			return "", fmt.Errorf("failed to encode cursor value %d: %w", i, err)
		}
		encoded[i] = s
	}

	orderFields := make([]TokenOrder, len(orderBy.Fields))
	for i, f := range orderBy.Fields {
		orderFields[i] = TokenOrder{
			Field: f.Path,
			Desc:  f.Direction == Desc,
		}
	}

	token := &PageToken{
		CreateTime:   time.Now().UnixNano(),
		ResourceType: resourceType,
		FilterHash:   hashFilter(filter),
		CursorValues: encoded,
		OrderFields:  orderFields,
	}

	tokenBytes, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("failed to marshal page token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(tokenBytes), nil
}

// DecodeToken decodes a base64-encoded page token string.
// Returns (nil, nil) for empty token strings (first page request).
func DecodeToken(tokenStr string) (*PageToken, error) {
	if tokenStr == "" {
		return nil, nil //nolint:nilnil // Empty token is valid (first page)
	}

	tokenBytes, err := base64.RawURLEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid page token: malformed base64: %w", err)
	}

	var token PageToken
	if err := json.Unmarshal(tokenBytes, &token); err != nil {
		return nil, fmt.Errorf("invalid page token: malformed encoding: %w", err)
	}

	return &token, nil
}

// ValidateToken checks token expiration, resource type, and filter consistency.
func ValidateToken(token *PageToken, resourceType, filter string) error {
	if token == nil {
		return nil
	}

	createTime := time.Unix(0, token.CreateTime)
	if time.Since(createTime) > TokenMaxAge {
		return errTokenExpired
	}

	if token.ResourceType != resourceType {
		return errTokenWrongResource
	}

	expectedHash := hashFilter(filter)
	if token.FilterHash != expectedHash {
		return ErrFilterMismatch
	}

	return nil
}

// hashFilter creates a SHA256 hash of the filter string for consistency checks.
func hashFilter(filter string) string {
	if filter == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(filter))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}
