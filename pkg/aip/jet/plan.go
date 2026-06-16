package jet

import (
	"errors"
	"fmt"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

// BuildPlan validates AIP parameters and converts them into a neutral query plan.
func BuildPlan[M any](schema *Schema[M], params aip.Params) (*aip.Plan, error) {
	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = schema.defaultPageSize
	}
	if pageSize > schema.maxPageSize {
		pageSize = schema.maxPageSize
	}

	effectiveOrderBy, err := schema.effectiveOrderBy(params.OrderBy)
	if err != nil {
		return nil, wrapAIPError(err, aip.ErrInvalidOrderBy)
	}

	tok, err := aip.DecodeToken(params.PageToken)
	if err != nil {
		return nil, wrapAIPError(err, aip.ErrInvalidPageToken)
	}

	if err := aip.ValidateToken(tok, schema.resourceType, params.Filter); err != nil {
		if errors.Is(err, aip.ErrFilterMismatch) {
			return nil, err
		}
		return nil, wrapAIPError(err, aip.ErrInvalidPageToken)
	}

	cursorValues, err := schema.decodeCursorValues(tok, effectiveOrderBy)
	if err != nil {
		return nil, wrapAIPError(err, aip.ErrInvalidPageToken)
	}

	return &aip.Plan{
		PageSize:     pageSize,
		Filter:       params.Filter,
		OrderBy:      effectiveOrderBy,
		CursorValues: cursorValues,
	}, nil
}

func wrapAIPError(err, sentinel error) error {
	if errors.Is(err, sentinel) {
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}
