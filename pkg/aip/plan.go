package aip

// Params captures the standard AIP-158/AIP-132 list request fields.
type Params struct {
	PageSize  int32
	PageToken string
	Filter    string // Passed through opaquely; only hashed for token consistency checks.
	OrderBy   string // AIP-132 syntax: "field_name [asc|desc], ..."
}

// Plan is the validated, backend-neutral result of processing a list request's
// AIP parameters. After BuildPlan succeeds, all inputs have been checked:
// ordering is valid, page token is authentic and unexpired, filter hasn't
// changed.
type Plan struct {
	PageSize     int32
	Filter       string
	OrderBy      OrderBy
	CursorValues []any
}
