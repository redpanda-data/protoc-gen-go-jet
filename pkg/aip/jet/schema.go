package jet

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-jet/jet/v2/postgres"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

// Field defines a single sortable/pageable field for a resource model.
//
// Each field represents one API-level field name (e.g. "display_name") that
// clients can use in order_by and that participates in cursor-based pagination.
//
// Orderable fields must be non-nullable at the database layer, or the caller's
// expression must normalize NULLs (e.g. COALESCE). Tuple comparison is
// undefined when any element is NULL.
type Field[Model any] struct {
	// Column is the go-jet column reference used to generate ORDER BY clauses
	// and keyset WHERE predicates (e.g. table.MyResource.DisplayName).
	Column postgres.Column

	// Codec converts this field's values to/from the opaque page token.
	Codec aip.CursorCodec

	// DisableOrdering marks fields that can appear in the schema but must not
	// be used in the order_by parameter.
	DisableOrdering bool

	// GetValue extracts this field's current value from a scanned database row.
	// Called on the last row of each page to capture the cursor position for the
	// next page token.
	GetValue func(m *Model) any
}

// Fields maps API field paths to their Field definitions. The keys are the
// field names that clients use in the order_by query parameter (e.g.
// "display_name", "create_time"), not the database column names.
type Fields[Model any] map[string]Field[Model]

// Schema defines the ordering and pagination behaviour for a resource type.
type Schema[Model any] struct {
	resourceType     string
	fields           Fields[Model]
	defaultPageSize  int32
	maxPageSize      int32
	defaultOrder     []aip.OrderField
	tieBreakerFields []aip.OrderField

	// Cached at construction time (schema is immutable).
	cachedCodecs        map[string]aip.CursorCodec
	cachedAllowedFields []string
}

// Option configures a Schema via NewSchema.
type Option func(*schemaBuilder)

type schemaBuilder struct {
	defaultPageSize  int32
	maxPageSize      int32
	defaultOrder     []aip.OrderField
	tieBreakerFields []aip.OrderField
}

// WithDefaultPageSize sets the page size used when the caller sends 0 or negative.
func WithDefaultPageSize(n int32) Option {
	return func(b *schemaBuilder) { b.defaultPageSize = n }
}

// WithMaxPageSize sets the upper bound for page size. Requests exceeding
// this value are silently clamped (AIP-158).
func WithMaxPageSize(n int32) Option {
	return func(b *schemaBuilder) { b.maxPageSize = n }
}

// WithDefaultOrder sets a default ordering field used when the request has no order_by.
func WithDefaultOrder(fieldPath string, dir aip.SortDirection) Option {
	return func(b *schemaBuilder) {
		b.defaultOrder = append(b.defaultOrder, aip.OrderField{Path: fieldPath, Direction: dir})
	}
}

// WithTieBreaker appends a tie-breaker field that is always added to the end
// of every ORDER BY clause. This ensures deterministic pagination: without a
// unique tie-breaker, rows with identical sort values can appear on multiple
// pages or be skipped entirely. Typically the primary key (e.g. "id").
func WithTieBreaker(fieldPath string, dir aip.SortDirection) Option {
	return func(b *schemaBuilder) {
		b.tieBreakerFields = append(b.tieBreakerFields, aip.OrderField{Path: fieldPath, Direction: dir})
	}
}

// NewSchema creates a new Schema for the given resource type and fields.
// Panics if the configuration is invalid (same pattern as regexp.MustCompile).
func NewSchema[Model any](resourceType string, fields Fields[Model], opts ...Option) *Schema[Model] {
	b := &schemaBuilder{
		defaultPageSize: 50,
		maxPageSize:     1000,
	}
	for _, o := range opts {
		o(b)
	}

	s := &Schema[Model]{
		resourceType:     resourceType,
		fields:           fields,
		defaultPageSize:  b.defaultPageSize,
		maxPageSize:      b.maxPageSize,
		defaultOrder:     b.defaultOrder,
		tieBreakerFields: b.tieBreakerFields,
	}

	if err := s.validate(); err != nil {
		panic(fmt.Sprintf("aip: invalid schema for %s: %v", resourceType, err)) //nolint:forbidigo // init-time validation, same pattern as regexp.MustCompile
	}

	// Pre-compute immutable caches.
	s.cachedCodecs = s.buildCodecs()
	s.cachedAllowedFields = slices.Sorted(maps.Keys(s.fields))

	return s
}

// validate checks schema configuration at construction time.
func (s *Schema[M]) validate() error {
	if s.defaultPageSize < 1 {
		return fmt.Errorf("defaultPageSize must be >= 1, got %d", s.defaultPageSize)
	}

	if s.maxPageSize < 1 {
		return fmt.Errorf("maxPageSize must be >= 1, got %d", s.maxPageSize)
	}

	// cachedAllowedFields is populated by NewSchema AFTER validate
	// returns, so the cached accessor is empty here. Compute the list
	// locally so validation error messages can name the fields the
	// author actually declared, not an empty slice.
	allowed := slices.Sorted(maps.Keys(s.fields))

	for _, f := range s.defaultOrder {
		field, ok := s.fields[f.Path]
		if !ok {
			return fmt.Errorf("default order field %q not in schema (allowed: %s)", f.Path, strings.Join(allowed, ", "))
		}
		if field.DisableOrdering {
			return fmt.Errorf("default order field %q has ordering disabled", f.Path)
		}
	}

	for _, f := range s.tieBreakerFields {
		field, ok := s.fields[f.Path]
		if !ok {
			return fmt.Errorf("tie-breaker field %q not in schema (allowed: %s)", f.Path, strings.Join(allowed, ", "))
		}
		if field.DisableOrdering {
			return fmt.Errorf("tie-breaker field %q has ordering disabled", f.Path)
		}
	}

	for path, field := range s.fields {
		if field.DisableOrdering {
			continue
		}
		if field.Codec == nil {
			return fmt.Errorf("orderable field %q is missing a Codec", path)
		}
		if field.GetValue == nil {
			return fmt.Errorf("orderable field %q is missing a GetValue function", path)
		}
		if field.Column == nil {
			return fmt.Errorf("orderable field %q is missing a Column", path)
		}
	}

	return nil
}

// codecs returns the cached map of field path -> CursorCodec for token encoding.
func (s *Schema[M]) codecs() map[string]aip.CursorCodec {
	return s.cachedCodecs
}

// buildCodecs constructs the codecs map once at schema creation time.
func (s *Schema[M]) buildCodecs() map[string]aip.CursorCodec {
	codecs := make(map[string]aip.CursorCodec, len(s.fields))
	for path, field := range s.fields {
		if field.Codec != nil {
			codecs[path] = field.Codec
		}
	}
	return codecs
}

// allowedFields returns the cached sorted field paths for validation and error messages.
func (s *Schema[M]) allowedFields() []string {
	return s.cachedAllowedFields
}

// effectiveOrderBy resolves the complete ordering that will be used in the
// database query. If the client provided an order_by, it is used; otherwise
// the schema's defaults apply. Tie-breaker fields are always appended (unless
// already present) to guarantee deterministic pagination order.
func (s *Schema[M]) effectiveOrderBy(orderByStr string) (aip.OrderBy, error) {
	ob, err := aip.ParseOrderBy(orderByStr)
	if err != nil {
		return aip.OrderBy{}, err
	}

	var effective []aip.OrderField

	if len(ob.Fields) == 0 {
		effective = append(effective, s.defaultOrder...)
	} else {
		effective = append(effective, ob.Fields...)
	}

	// Validate only the client-supplied fields — defaults and tie-breakers
	// were already proven valid at schema construction time.
	for _, f := range ob.Fields {
		field, ok := s.fields[f.Path]
		if !ok {
			return aip.OrderBy{}, aip.NewFieldError("order_by", f.Path, s.allowedFields())
		}
		if field.DisableOrdering {
			return aip.OrderBy{}, aip.NewFieldError("order_by", f.Path, s.allowedFields())
		}
	}

	effective = aip.AppendUniqueFields(effective, s.tieBreakerFields...)

	return aip.OrderBy{Fields: effective}, nil
}

// decodeCursorValues validates that the page token's ordering matches the
// current request's ordering, then decodes the cursor values from the token
// back into Go types.
func (s *Schema[M]) decodeCursorValues(
	token *aip.PageToken,
	orderBy aip.OrderBy,
) ([]any, error) {
	if token == nil || len(token.OrderFields) == 0 {
		return nil, nil
	}

	if len(token.CursorValues) != len(token.OrderFields) {
		return nil, fmt.Errorf("cursor values count (%d) does not match order fields count (%d)",
			len(token.CursorValues), len(token.OrderFields))
	}

	if len(orderBy.Fields) != len(token.OrderFields) {
		return nil, aip.ErrTokenOrderChanged
	}

	for i, tf := range token.OrderFields {
		ef := orderBy.Fields[i]
		if tf.Field != ef.Path || tf.Desc != (ef.Direction == aip.Desc) {
			return nil, aip.ErrTokenOrderChanged
		}
	}

	values := make([]any, len(token.CursorValues))
	for i, v := range token.CursorValues {
		name := token.OrderFields[i].Field

		f, ok := s.fields[name]
		if !ok {
			return nil, fmt.Errorf("cursor decode for %q: field not in schema", name)
		}

		goVal, err := f.Codec.Decode(v)
		if err != nil {
			return nil, fmt.Errorf("cursor decode for %q: %w", name, err)
		}
		values[i] = goVal
	}

	return values, nil
}

// extractCursorValues reads the ordered field values from the last row of
// the current page. These values become the cursor in the next page token.
func (s *Schema[M]) extractCursorValues(row *M, orderBy aip.OrderBy) ([]any, error) {
	values := make([]any, len(orderBy.Fields))
	for i, field := range orderBy.Fields {
		f, ok := s.fields[field.Path]
		if !ok {
			return nil, aip.NewFieldError("extract cursor", field.Path, s.allowedFields())
		}
		values[i] = f.GetValue(row)
	}
	return values, nil
}
