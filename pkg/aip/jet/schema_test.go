package jet

import (
	"testing"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
)

type testModel struct {
	ID          string
	DisplayName string
	CreatedAt   time.Time
}

var (
	testIDCol          = postgres.StringColumn("id")
	testDisplayNameCol = postgres.StringColumn("display_name")
	testCreatedAtCol   = postgres.TimestampzColumn("created_at")
)

func newTestSchema(opts ...Option) *Schema[testModel] {
	defaultOpts := []Option{
		WithDefaultOrder("created_at", aip.Desc),
		WithTieBreaker("id", aip.Asc),
	}
	return NewSchema[testModel](
		"test/TestModel",
		Fields[testModel]{
			"id": {
				Column: testIDCol,
				Codec:  aip.StringCodec{},
				GetValue: func(m *testModel) any {
					return m.ID
				},
			},
			"display_name": {
				Column: testDisplayNameCol,
				Codec:  aip.StringCodec{},
				GetValue: func(m *testModel) any {
					return m.DisplayName
				},
			},
			"created_at": {
				Column: testCreatedAtCol,
				Codec:  aip.TimestampCodec{},
				GetValue: func(m *testModel) any {
					return m.CreatedAt
				},
			},
		},
		append(defaultOpts, opts...)...,
	)
}

func TestNewSchema_Defaults(t *testing.T) {
	s := newTestSchema()
	assert.Equal(t, int32(50), s.defaultPageSize)
	assert.Equal(t, int32(1000), s.maxPageSize)
}

func TestNewSchema_CustomPageSizes(t *testing.T) {
	s := newTestSchema(WithDefaultPageSize(10), WithMaxPageSize(100))
	assert.Equal(t, int32(10), s.defaultPageSize)
	assert.Equal(t, int32(100), s.maxPageSize)
}

func TestNewSchema_PanicsOnMissingCodec(t *testing.T) {
	assert.Panics(t, func() {
		NewSchema[testModel]("test/Bad", Fields[testModel]{
			"id": {
				Column:   testIDCol,
				GetValue: func(m *testModel) any { return m.ID },
				// Missing Codec
			},
		})
	})
}

func TestNewSchema_PanicsOnMissingGetValue(t *testing.T) {
	assert.Panics(t, func() {
		NewSchema[testModel]("test/Bad", Fields[testModel]{
			"id": {
				Column: testIDCol,
				Codec:  aip.StringCodec{},
				// Missing GetValue
			},
		})
	})
}

func TestNewSchema_PanicsOnMissingColumn(t *testing.T) {
	assert.Panics(t, func() {
		NewSchema[testModel]("test/Bad", Fields[testModel]{
			"id": {
				Codec:    aip.StringCodec{},
				GetValue: func(m *testModel) any { return m.ID },
				// Missing Column
			},
		})
	})
}

func TestNewSchema_PanicsOnInvalidDefaultOrder(t *testing.T) {
	assert.Panics(t, func() {
		NewSchema[testModel]("test/Bad", Fields[testModel]{
			"id": {Column: testIDCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.ID }},
		}, WithDefaultOrder("nonexistent", aip.Asc))
	})
}

// TestNewSchema_ValidationErrorNamesTheFields pins the invariant the
// caller of a bad schema relies on: the panic message actually lists
// the valid field paths. validate() runs before cachedAllowedFields is
// populated, so the error-formatting branch had to compute the list
// itself. A regression here would leave authors seeing "allowed: "
// with no fields, which is useless on a typo.
func TestNewSchema_ValidationErrorNamesTheFields(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "schema with bad default order must panic")
		msg, ok := r.(string)
		require.Truef(t, ok, "panic payload should be the formatted string, got %T", r)
		assert.Contains(t, msg, "id")
		assert.Contains(t, msg, "display_name")
	}()
	NewSchema[testModel]("test/Bad", Fields[testModel]{
		"id":           {Column: testIDCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.ID }},
		"display_name": {Column: testDisplayNameCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.DisplayName }},
	}, WithDefaultOrder("nonexistent", aip.Asc))
}

func TestNewSchema_PanicsOnInvalidTieBreaker(t *testing.T) {
	assert.Panics(t, func() {
		NewSchema[testModel]("test/Bad", Fields[testModel]{
			"id": {Column: testIDCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.ID }},
		}, WithTieBreaker("nonexistent", aip.Asc))
	})
}

func TestNewSchema_DisabledOrderingSkipsValidation(t *testing.T) {
	// Fields with DisableOrdering don't need Codec/GetValue/Column
	assert.NotPanics(t, func() {
		NewSchema[testModel]("test/Ok", Fields[testModel]{
			"id":       {Column: testIDCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.ID }},
			"metadata": {DisableOrdering: true},
		}, WithTieBreaker("id", aip.Asc))
	})
}

func TestEffectiveOrderBy_EmptyUsesDefaults(t *testing.T) {
	s := newTestSchema()
	ob, err := s.effectiveOrderBy("")
	require.NoError(t, err)

	// Default: created_at desc + tie-breaker: id asc
	require.Len(t, ob.Fields, 2)
	assert.Equal(t, "created_at", ob.Fields[0].Path)
	assert.Equal(t, aip.Desc, ob.Fields[0].Direction)
	assert.Equal(t, "id", ob.Fields[1].Path)
	assert.Equal(t, aip.Asc, ob.Fields[1].Direction)
}

func TestEffectiveOrderBy_UserOrderPlusTieBreaker(t *testing.T) {
	s := newTestSchema()
	ob, err := s.effectiveOrderBy("display_name asc")
	require.NoError(t, err)

	require.Len(t, ob.Fields, 2)
	assert.Equal(t, "display_name", ob.Fields[0].Path)
	assert.Equal(t, "id", ob.Fields[1].Path) // tie-breaker appended
}

func TestEffectiveOrderBy_TieBreakerNotDuplicated(t *testing.T) {
	s := newTestSchema()
	ob, err := s.effectiveOrderBy("id desc")
	require.NoError(t, err)

	require.Len(t, ob.Fields, 1) // "id" not duplicated
	assert.Equal(t, "id", ob.Fields[0].Path)
	assert.Equal(t, aip.Desc, ob.Fields[0].Direction) // user's direction preserved
}

func TestEffectiveOrderBy_InvalidFieldRejected(t *testing.T) {
	s := newTestSchema()
	_, err := s.effectiveOrderBy("nonexistent asc")
	assert.ErrorIs(t, err, aip.ErrInvalidOrderBy)
}

func TestEffectiveOrderBy_DisabledFieldRejected(t *testing.T) {
	s := NewSchema[testModel]("test/WithDisabled", Fields[testModel]{
		"id":       {Column: testIDCol, Codec: aip.StringCodec{}, GetValue: func(m *testModel) any { return m.ID }},
		"metadata": {Column: testDisplayNameCol, Codec: aip.StringCodec{}, DisableOrdering: true, GetValue: func(m *testModel) any { return "" }},
	}, WithTieBreaker("id", aip.Asc))

	_, err := s.effectiveOrderBy("metadata asc")
	assert.ErrorIs(t, err, aip.ErrInvalidOrderBy)
}
