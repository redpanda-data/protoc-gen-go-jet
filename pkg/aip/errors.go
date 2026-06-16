package aip

import "errors"

// Sentinel errors for list query validation failures.
var (
	// ErrInvalidOrderBy indicates that the order_by parameter contains invalid field names or syntax.
	ErrInvalidOrderBy = errors.New("invalid order_by parameter")

	// ErrInvalidPageToken indicates that the page_token parameter is malformed or invalid.
	ErrInvalidPageToken = errors.New("invalid page_token parameter")

	// ErrFilterMismatch indicates that the filter parameter doesn't match the one used to create the page token.
	ErrFilterMismatch = errors.New("filter parameter mismatch with page token")
)

// Sub-causes for token validation.
var (
	errTokenExpired       = errors.New("page token has expired")
	errTokenWrongResource = errors.New("page token is for wrong resource type")
	// ErrTokenOrderChanged indicates the order_by changed since the token was issued.
	ErrTokenOrderChanged = errors.New("order_by changed since token was issued")
)
