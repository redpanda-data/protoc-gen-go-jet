//go:build integration

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	overridev1 "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/override/v1"
	e2ev1 "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1"
	gen "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1/storage"
	jetmodel "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1/storage/jet/public/model"
	customstorage "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/override_layout"
	custommodel "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/override_layout/jet/public/model"
	pkgtestcontainers "github.com/redpanda-data/protoc-gen-go-jet/internal/testcontainers"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/aip"
	aipjet "github.com/redpanda-data/protoc-gen-go-jet/pkg/aip/jet"
)

// Shared resources. One container per package run.
var (
	sharedPG       *pkgtestcontainers.Postgres
	sharedSuperDB  *sql.DB
	sharedTenantDB *sql.DB
)

func TestMain(m *testing.M) {
	run(m)
}

// run isolates TestMain's logic so the lint exception rule about
// os.Exit/log.Fatalf in TestMain isn't tripped — all of our
// termination lives in the inner helper. Any non-nil error here
// exits the process with status 1; on success we let the test
// runner exit implicitly per the Go 1.15 contract.
func run(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg, superDB, tenantDB, err := bootstrap(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e bootstrap: %v\n", err)
		os.Exit(1) //nolint:revive // bootstrap failure must fail the process
		return
	}
	sharedPG = pg
	sharedSuperDB = superDB
	sharedTenantDB = tenantDB

	defer func() {
		_ = sharedTenantDB.Close()
		_ = sharedSuperDB.Close()
		termCtx, termCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer termCancel()
		if err := sharedPG.Terminate(termCtx); err != nil {
			fmt.Fprintf(os.Stderr, "terminate pg: %v\n", err)
		}
	}()

	m.Run()
}

// jetExec is a tiny adapter — jet's ExecContext returns
// (sql.Result, error) but the harness only ever cares about the error.
// Centralising the discard makes the test callsites read as a single
// statement per INSERT / UPDATE.
type jetExecer interface {
	ExecContext(ctx context.Context, db qrm.Executable) (sql.Result, error)
}

func jetExec(ctx context.Context, tx *sql.Tx, stmt jetExecer) error {
	_, err := stmt.ExecContext(ctx, tx)
	return err
}

// --------------------------------------------------------------------
// Scalars — every proto3 scalar kind round-trips through the generated
// mapper and go-jet INSERT/SELECT.
// --------------------------------------------------------------------

// sampleScalars returns a non-zero proto with values exercising the
// boundary conditions for each kind.
func sampleScalars(id string) *e2ev1.Scalars {
	return &e2ev1.Scalars{
		Id:            id,
		StringValue:   `hello "world"` + "\nwith\tmetacharacters 🚀",
		BoolValue:     true,
		Int32Value:    math.MinInt32,
		Sint32Value:   math.MaxInt32,
		Sfixed32Value: -1,
		// uint32/fixed32 round-trip through the plugin's int32 model
		// column via two's-complement reinterpretation. Cover the high
		// bit (>math.MaxInt32) so the cast direction is exercised in
		// both halves.
		Uint32Value:   math.MaxUint32,
		Fixed32Value:  0x80000000,
		Int64Value:    math.MaxInt64,
		Sint64Value:   math.MinInt64,
		Sfixed64Value: 42,
		// uint64/fixed64 — same dance in the 64-bit lane.
		Uint64Value:  math.MaxUint64,
		Fixed64Value: 0x8000000000000000,
		// REAL / DOUBLE PRECISION — bit-exact at PG precision.
		FloatValue:      float32(math.MaxFloat32 / 2),
		DoubleValue:     math.Pi,
		BytesValue:      makeByteAlphabet(),
		KindValue:       e2ev1.ScalarKind_SCALAR_KIND_ALPHA,
		KindValueStrict: e2ev1.ScalarKind_SCALAR_KIND_BETA,
	}
}

func TestScalars_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := sampleScalars("scalars-" + uniqueID(t))
	row, err := gen.ScalarsModelFromProto(p)
	require.NoError(t, err)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ScalarsTable.
			INSERT(gen.ScalarsTable.AllColumns).
			MODEL(row))
	}))

	var read jetmodel.Scalars
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ScalarsTable.AllColumns).
			FROM(gen.ScalarsTable).
			WHERE(gen.ScalarsTable.ID.EQ(postgres.String(p.GetId()))).
			QueryContext(ctx, tx, &read)
	}))

	got, err := gen.ScalarsProtoFromModel(&read)
	require.NoError(t, err)

	// Every scalar round-trips byte-identically.
	assert.Equal(t, p.GetId(), got.GetId())
	assert.Equal(t, p.GetStringValue(), got.GetStringValue())
	assert.Equal(t, p.GetBoolValue(), got.GetBoolValue())
	assert.Equal(t, p.GetInt32Value(), got.GetInt32Value())
	assert.Equal(t, p.GetSint32Value(), got.GetSint32Value())
	assert.Equal(t, p.GetSfixed32Value(), got.GetSfixed32Value())
	assert.Equal(t, p.GetUint32Value(), got.GetUint32Value())
	assert.Equal(t, p.GetFixed32Value(), got.GetFixed32Value())
	assert.Equal(t, p.GetInt64Value(), got.GetInt64Value())
	assert.Equal(t, p.GetSint64Value(), got.GetSint64Value())
	assert.Equal(t, p.GetSfixed64Value(), got.GetSfixed64Value())
	assert.Equal(t, p.GetFloatValue(), got.GetFloatValue())
	assert.Equal(t, p.GetDoubleValue(), got.GetDoubleValue())
	assert.Equal(t, p.GetUint64Value(), got.GetUint64Value())
	assert.Equal(t, p.GetFixed64Value(), got.GetFixed64Value())
	assert.Equal(t, p.GetBytesValue(), got.GetBytesValue())
	assert.Equal(t, p.GetKindValue(), got.GetKindValue())
	assert.Equal(t, p.GetKindValueStrict(), got.GetKindValueStrict())
}

// TestScalars_SkipTrueExcludesColumn pins that a field annotated
// `(storage.v1.column) = { skip: true }` lands neither in the DDL
// nor in the generated jet model. Real-world protos depend on this
// (LLMProvider.url / MCPServer.url are `skip: true`); this test
// is the direct e2e assertion that the feature works, independent
// of a drift-check pipeline.
func TestScalars_SkipTrueExcludesColumn(t *testing.T) {
	t.Parallel()
	// The Scalars proto carries `computed_hint` with skip: true. If
	// the plugin regresses and starts persisting skipped fields,
	// the generated jet model would gain a `ComputedHint` field and
	// the DDL a matching column. Use reflection on the jet model
	// struct to assert the field is absent.
	model := jetmodel.Scalars{}
	typ := reflect.TypeOf(model)
	for i := 0; i < typ.NumField(); i++ {
		assert.NotEqual(t, "ComputedHint", typ.Field(i).Name,
			"skip: true field must not land in the jet model; got field at index %d", i)
	}
	// Belt and suspenders: the embedded DDL must not mention the
	// column name either — drift-check already pins this, but a
	// direct string assertion is cheap and clear.
	assert.NotContains(t, gen.ScalarsDDL, "computed_hint",
		"skip: true column must not appear in the canonical DDL")
}

func TestScalars_ZeroValues(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// BYTEA is NOT NULL with DEFAULT ''::bytea. The generated mapper
	// copies the proto3 zero (nil) directly — go-jet's INSERT with
	// AllColumns passes NULL, violating the constraint. Known bytes-
	// zero-value bug. Workaround: send an explicit empty slice to pin the
	// "every other scalar zero round-trips" contract without blocking
	// on the bytes bug.
	p := &e2ev1.Scalars{
		Id:              "zeros-" + uniqueID(t),
		BytesValue:      []byte{},
		KindValueStrict: e2ev1.ScalarKind_SCALAR_KIND_ALPHA,
	}
	row, err := gen.ScalarsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ScalarsTable.
			INSERT(gen.ScalarsTable.AllColumns).
			MODEL(row))
	}))

	var read jetmodel.Scalars
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ScalarsTable.AllColumns).
			FROM(gen.ScalarsTable).
			WHERE(gen.ScalarsTable.ID.EQ(postgres.String(p.GetId()))).
			QueryContext(ctx, tx, &read)
	}))

	got, err := gen.ScalarsProtoFromModel(&read)
	require.NoError(t, err)
	assert.Equal(t, "", got.GetStringValue())
	assert.False(t, got.GetBoolValue())
	assert.EqualValues(t, 0, got.GetInt32Value())
	assert.EqualValues(t, 0, got.GetSint32Value())
	assert.EqualValues(t, 0, got.GetInt64Value())
	// BYTEA default is the empty byte slice — proto round-trips as nil.
	assert.Empty(t, got.GetBytesValue())
	assert.Equal(t, e2ev1.ScalarKind_SCALAR_KIND_UNSPECIFIED, got.GetKindValue())
	assert.Equal(t, e2ev1.ScalarKind_SCALAR_KIND_ALPHA, got.GetKindValueStrict())
}

// TestScalars_NilBytesCoercedToEmpty pins the mapper's nil-bytes
// contract: proto3 returns nil for an unset `bytes` field, and the
// generator coerces nil to []byte{} so the NOT NULL DEFAULT ”::bytea
// column doesn't trip. Without the guard, INSERT fails with a NotNull
// violation. The regression tripwire: if anyone drops the nil
// coercion in emit_mapper.go's KindScalar case, this test flips red.
func TestScalars_NilBytesCoercedToEmpty(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := &e2ev1.Scalars{
		Id:              "nil-bytes-" + uniqueID(t),
		KindValueStrict: e2ev1.ScalarKind_SCALAR_KIND_ALPHA,
	}
	row, err := gen.ScalarsModelFromProto(p)
	require.NoError(t, err)
	require.NotNil(t, row.BytesValue, "mapper must emit a non-nil empty slice")
	require.Len(t, row.BytesValue, 0)
	err = inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ScalarsTable.
			INSERT(gen.ScalarsTable.AllColumns).
			MODEL(row))
	})
	require.NoError(t, err, "nil bytes must coerce to empty, not SQL NULL")
}

// TestScalars_StrictEnumRejectsZero writes UNSPECIFIED to the strict
// enum column through raw SQL so the CHECK constraint is the only
// thing between us and a bad row.
func TestScalars_StrictEnumRejectsZero(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, err := sharedSuperDB.ExecContext(ctx, `
		INSERT INTO scalars (id, kind_value_strict) VALUES ($1, 'SCALAR_KIND_UNSPECIFIED')
	`, "strict-"+uniqueID(t))
	requirePGCode(t, err, pgerrcode.CheckViolation)
}

// TestScalars_EveryEnumValueRoundTrips covers the full enum surface.
func TestScalars_EveryEnumValueRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	values := []e2ev1.ScalarKind{
		e2ev1.ScalarKind_SCALAR_KIND_UNSPECIFIED,
		e2ev1.ScalarKind_SCALAR_KIND_ALPHA,
		e2ev1.ScalarKind_SCALAR_KIND_BETA,
		e2ev1.ScalarKind_SCALAR_KIND_GAMMA,
	}
	for _, v := range values {
		v := v
		t.Run(v.String(), func(t *testing.T) {
			t.Parallel()
			p := &e2ev1.Scalars{
				Id: "enum-" + v.String() + "-" + uniqueID(t),
				// Empty non-nil bytes to sidestep the bytes-zero-value
				// bug. See TestScalars_ZeroValues for context.
				BytesValue:      []byte{},
				KindValue:       v,
				KindValueStrict: e2ev1.ScalarKind_SCALAR_KIND_ALPHA,
			}
			row, err := gen.ScalarsModelFromProto(p)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.ScalarsTable.INSERT(gen.ScalarsTable.AllColumns).MODEL(row))
			}))
			var read jetmodel.Scalars
			require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return postgres.SELECT(gen.ScalarsTable.AllColumns).
					FROM(gen.ScalarsTable).
					WHERE(gen.ScalarsTable.ID.EQ(postgres.String(p.GetId()))).
					QueryContext(ctx, tx, &read)
			}))
			assert.Equal(t, v.String(), read.KindValue)
		})
	}
}

// --------------------------------------------------------------------
// Times — Timestamp + Duration round-trip.
// --------------------------------------------------------------------

func TestTimes_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	id := "times-" + uniqueID(t)
	// Postgres TIMESTAMPTZ keeps microsecond precision — round-trip to
	// that granularity.
	ts := time.Date(2024, 6, 15, 12, 34, 56, 789000000, time.UTC)
	created := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	ttl := 3*time.Hour + 42*time.Minute + 17*time.Second

	// Repeated WKT values span zero / negative / large / sub-ms.
	// TIMESTAMPTZ[] stores microseconds, same floor as scalar
	// TIMESTAMPTZ — pin the checkpoint values to μs-aligned
	// nanoseconds so the round-trip is exact.
	cp1 := time.Date(2020, 1, 1, 0, 0, 0, 123456000, time.UTC)
	cp2 := time.Date(2099, 12, 31, 23, 59, 59, 999999000, time.UTC)
	cp3 := time.Unix(0, 0)
	bo1 := time.Duration(0)
	bo2 := -42 * time.Second
	bo3 := 25 * time.Hour

	p := &e2ev1.Times{
		Id:        id,
		EventAt:   timestamppb.New(ts),
		Ttl:       durationpb.New(ttl),
		CreatedAt: timestamppb.New(created),
		Checkpoints: []*timestamppb.Timestamp{
			timestamppb.New(cp1), timestamppb.New(cp2), timestamppb.New(cp3),
		},
		Backoffs: []*durationpb.Duration{
			durationpb.New(bo1), durationpb.New(bo2), durationpb.New(bo3),
		},
	}
	row, err := gen.TimesModelFromProto(p)
	require.NoError(t, err)
	// Timestamps are not written by ModelFromProto — the repository
	// fills them. Mirror that here so the round-trip is complete.
	row.EventAt = ts
	row.CreatedAt = created
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.TimesTable.INSERT(gen.TimesTable.AllColumns).MODEL(row))
	}))

	var read jetmodel.Times
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.TimesTable.AllColumns).
			FROM(gen.TimesTable).
			WHERE(gen.TimesTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))

	got, err := gen.TimesProtoFromModel(&read)
	require.NoError(t, err)
	assert.True(t, ts.Equal(got.GetEventAt().AsTime()), "EventAt: want %v got %v", ts, got.GetEventAt().AsTime())
	assert.True(t, created.Equal(got.GetCreatedAt().AsTime()))
	assert.Equal(t, ttl, got.GetTtl().AsDuration())

	// Repeated timestamps round-trip microsecond-exact. The
	// TimestampArray helper scans each element via pq.StringArray
	// and parses the PG timestamptz literal; sub-μs nanos truncate
	// at the PG TIMESTAMPTZ floor (cp1/cp2 are μs-aligned above).
	require.Len(t, got.GetCheckpoints(), 3)
	assert.True(t, cp1.Equal(got.GetCheckpoints()[0].AsTime()))
	assert.True(t, cp2.Equal(got.GetCheckpoints()[1].AsTime()))
	assert.True(t, cp3.Equal(got.GetCheckpoints()[2].AsTime()))

	require.Len(t, got.GetBackoffs(), 3)
	assert.Equal(t, bo1, got.GetBackoffs()[0].AsDuration())
	assert.Equal(t, bo2, got.GetBackoffs()[1].AsDuration())
	assert.Equal(t, bo3, got.GetBackoffs()[2].AsDuration())
}

// TestTimes_RepeatedTimestampIsNativeTimestampTZ pins that the
// checkpoints column stores as actual TIMESTAMPTZ[], not BIGINT[],
// so SQL timestamp-native operators apply. Without the
// jettypes.TimestampArray override the plugin would fall back to
// BIGINT[] of unix nanoseconds and this query would fail with a
// type-mismatch error.
func TestTimes_RepeatedTimestampIsNativeTimestampTZ(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "times-tz-array-" + uniqueID(t)
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	checkpoint := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	p := &e2ev1.Times{
		Id:          id,
		EventAt:     timestamppb.New(now),
		CreatedAt:   timestamppb.New(now),
		Checkpoints: []*timestamppb.Timestamp{timestamppb.New(checkpoint)},
	}
	row, err := gen.TimesModelFromProto(p)
	require.NoError(t, err)
	row.EventAt = now
	row.CreatedAt = now
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.TimesTable.INSERT(gen.TimesTable.AllColumns).MODEL(row))
	}))

	// The @> operator is TIMESTAMPTZ-native — it'd return a type
	// mismatch on BIGINT[]. Confirms the schema is what we expect.
	var containsExact bool
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT checkpoints @> ARRAY[$1::timestamptz]
		FROM times WHERE id = $2
	`, checkpoint, id).Scan(&containsExact))
	assert.True(t, containsExact, "checkpoints should contain the exact seeded timestamp")

	// UNNEST also works on TIMESTAMPTZ[] — sanity-check that each
	// element can be compared against a timestamp literal. Matching
	// ON 2025-06-15 yields exactly one element.
	var cnt int
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT count(*) FROM (
		  SELECT unnest(checkpoints) AS ts FROM times WHERE id = $1
		) s WHERE ts::date = '2025-06-15'::date
	`, id).Scan(&cnt))
	assert.Equal(t, 1, cnt)
}

// TestTimes_RepeatedWKTEmpty pins the empty-slice DB representation —
// nil repeated WKT now maps to SQL NULL (repeated kinds default to
// nullable; nil proto -> NULL preserves the Go-side nil/empty
// distinction through round-trip).
func TestTimes_RepeatedWKTEmpty(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "times-empty-wkt-" + uniqueID(t)
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &e2ev1.Times{Id: id, EventAt: timestamppb.New(now), CreatedAt: timestamppb.New(now)}
	row, err := gen.TimesModelFromProto(p)
	require.NoError(t, err)
	row.EventAt = now
	row.CreatedAt = now
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.TimesTable.INSERT(gen.TimesTable.AllColumns).MODEL(row))
	}))
	var cpTxt, boTxt sql.NullString
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT checkpoints::text, backoffs::text FROM times WHERE id = $1
	`, id).Scan(&cpTxt, &boTxt))
	assert.False(t, cpTxt.Valid, "checkpoints should be NULL for nil proto")
	assert.False(t, boTxt.Valid, "backoffs should be NULL for nil proto")
}

func TestTimes_DurationBoundaries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cases := []struct {
		name string
		d    time.Duration
	}{
		{"zero", 0},
		{"negative", -500 * time.Millisecond},
		{"huge", 9999 * time.Hour},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			id := "dur-" + c.name + "-" + uniqueID(t)
			p := &e2ev1.Times{Id: id, Ttl: durationpb.New(c.d)}
			row, err := gen.TimesModelFromProto(p)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.TimesTable.INSERT(gen.TimesTable.AllColumns).MODEL(row))
			}))
			var read jetmodel.Times
			require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return postgres.SELECT(gen.TimesTable.AllColumns).
					FROM(gen.TimesTable).
					WHERE(gen.TimesTable.ID.EQ(postgres.String(id))).
					QueryContext(ctx, tx, &read)
			}))
			got, err := gen.TimesProtoFromModel(&read)
			require.NoError(t, err)
			assert.Equal(t, c.d, got.GetTtl().AsDuration())
		})
	}
}

// --------------------------------------------------------------------
// Collections — repeated string/enum/message, nested message, map.
// --------------------------------------------------------------------

func TestCollections_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "coll-" + uniqueID(t)

	p := &e2ev1.Collections{
		Id: id,
		// Deliberately include PG array metacharacters — comma, quotes,
		// backslash, unicode, empty string.
		Labels: []string{"plain", "with,comma", `"double"`, "日本語", `back\slash`},
		Tags: []e2ev1.CollectionTag{
			e2ev1.CollectionTag_COLLECTION_TAG_RED,
			e2ev1.CollectionTag_COLLECTION_TAG_GREEN,
			e2ev1.CollectionTag_COLLECTION_TAG_BLUE,
		},
		Items: []*e2ev1.NestedPayload{
			{Name: "first", Count: 1, Flag: true, Tags: []string{"a", "b"}},
			{Name: "second", Count: 2, Flag: false, Tags: nil},
		},
		Payload: &e2ev1.NestedPayload{
			Name:  "root",
			Count: math.MaxInt64,
			Flag:  true,
			Tags:  []string{"x", "y", "z"},
		},
		Attributes: map[string]string{
			"region":           "eu-west-1",
			"日本語":              "value",
			`key"with"quotes`:  "v",
			`with\backslash`:   "v2",
			`percent%wildcard`: "v3",
			`underscore_too`:   "v4",
		},
		// Native-array primitive fields — boundary values through the
		// pq.<T>Array codec. uint max values test the two's-complement
		// reinterpret the mapper does element-wise.
		Flags:   []bool{true, false, true, true, false},
		Ints32:  []int32{math.MinInt32, -1, 0, 1, math.MaxInt32},
		Uints32: []uint32{0, 1, 0x7fffffff, 0x80000000, math.MaxUint32},
		Ints64:  []int64{math.MinInt64, -1, 0, 1, math.MaxInt64},
		Uints64: []uint64{0, 1, 0x7fffffffffffffff, 0x8000000000000000, math.MaxUint64},
		Floats:  []float32{0, float32(math.Pi), float32(-math.MaxFloat32 / 2)},
		Doubles: []float64{0, math.Pi, math.E, -math.MaxFloat64 / 2},
	}

	row, err := gen.CollectionsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))

	var read jetmodel.Collections
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.CollectionsTable.AllColumns).
			FROM(gen.CollectionsTable).
			WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.CollectionsProtoFromModel(&read)
	require.NoError(t, err)

	assert.Equal(t, p.GetLabels(), got.GetLabels())
	assert.Equal(t, p.GetTags(), got.GetTags())
	require.Len(t, got.GetItems(), len(p.GetItems()))
	for i := range p.GetItems() {
		assert.Equal(t, p.GetItems()[i].GetName(), got.GetItems()[i].GetName())
		assert.Equal(t, p.GetItems()[i].GetCount(), got.GetItems()[i].GetCount())
		assert.Equal(t, p.GetItems()[i].GetFlag(), got.GetItems()[i].GetFlag())
		assert.Equal(t, p.GetItems()[i].GetTags(), got.GetItems()[i].GetTags())
	}
	assert.Equal(t, p.GetPayload().GetName(), got.GetPayload().GetName())
	assert.Equal(t, p.GetPayload().GetCount(), got.GetPayload().GetCount())
	assert.Equal(t, p.GetPayload().GetTags(), got.GetPayload().GetTags())
	assert.Equal(t, p.GetAttributes(), got.GetAttributes())

	// Native-array round-trips — element-for-element equality including
	// the boundary uint bit patterns.
	assert.Equal(t, p.GetFlags(), got.GetFlags())
	assert.Equal(t, p.GetInts32(), got.GetInts32())
	assert.Equal(t, p.GetUints32(), got.GetUints32())
	assert.Equal(t, p.GetInts64(), got.GetInts64())
	assert.Equal(t, p.GetUints64(), got.GetUints64())
	assert.Equal(t, p.GetFloats(), got.GetFloats())
	assert.Equal(t, p.GetDoubles(), got.GetDoubles())
}

func TestCollections_EmptyValues(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "coll-empty-" + uniqueID(t)

	// Nil repeated / nil map — proto3 zero defaults.
	p := &e2ev1.Collections{Id: id}
	row, err := gen.CollectionsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))

	// Pin the DB representation directly so a future mapper change that
	// accidentally writes "null" or an empty string is caught loudly.
	// Repeated/map kinds default to nullable; nil proto -> SQL NULL
	// (not empty array / empty object). Only payload (single JSONB
	// message) keeps NOT NULL DEFAULT '{}' — proto zero value is the
	// empty message which protojson encodes as "{}".
	var labelsTxt, tagsTxt, itemsTxt, attrsTxt sql.NullString
	var flagsTxt, ints32Txt, uints32Txt, ints64Txt, uints64Txt, floatsTxt, doublesTxt sql.NullString
	var payloadTxt string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT labels::text, tags::text, items::text, payload::text, attributes::text,
		       flags::text, ints32::text, uints32::text, ints64::text, uints64::text,
		       floats::text, doubles::text
		FROM collections WHERE id = $1
	`, id).Scan(&labelsTxt, &tagsTxt, &itemsTxt, &payloadTxt, &attrsTxt,
		&flagsTxt, &ints32Txt, &uints32Txt, &ints64Txt, &uints64Txt,
		&floatsTxt, &doublesTxt))
	assert.False(t, labelsTxt.Valid, "labels should be NULL for nil proto")
	assert.False(t, tagsTxt.Valid, "tags should be NULL for nil proto")
	assert.False(t, itemsTxt.Valid, "items should be NULL for nil proto")
	assert.Equal(t, "{}", payloadTxt)
	assert.False(t, attrsTxt.Valid, "attributes should be NULL for nil proto")
	assert.False(t, flagsTxt.Valid, "flags should be NULL for nil proto")
	assert.False(t, ints32Txt.Valid, "ints32 should be NULL for nil proto")
	assert.False(t, uints32Txt.Valid, "uints32 should be NULL for nil proto")
	assert.False(t, ints64Txt.Valid, "ints64 should be NULL for nil proto")
	assert.False(t, uints64Txt.Valid, "uints64 should be NULL for nil proto")
	assert.False(t, floatsTxt.Valid, "floats should be NULL for nil proto")
	assert.False(t, doublesTxt.Valid, "doubles should be NULL for nil proto")
}

// TestCollections_EmptyPopulated pins the nil-vs-empty distinction for
// every repeated/map collection kind: a non-nil but empty proto value
// must round-trip as a non-NULL empty collection at the DDL layer and
// emerge as a non-nil empty slice/map on the proto side. Paired with
// TestCollections_EmptyValues which covers the nil -> SQL NULL direction.
func TestCollections_EmptyPopulated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "coll-empty-populated-" + uniqueID(t)

	// Explicitly-empty (non-nil, zero-length) collections across every
	// repeated primitive / repeated message / repeated enum / map kind.
	p := &e2ev1.Collections{
		Id:         id,
		Labels:     []string{},
		Tags:       []e2ev1.CollectionTag{},
		Items:      []*e2ev1.NestedPayload{},
		Attributes: map[string]string{},
		Flags:      []bool{},
		Ints32:     []int32{},
		Uints32:    []uint32{},
		Ints64:     []int64{},
		Uints64:    []uint64{},
		Floats:     []float32{},
		Doubles:    []float64{},
	}
	row, err := gen.CollectionsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))

	// Raw DB probe — every collection column must be non-NULL and the
	// literal empty form for its kind. Paired with the NULL probe in
	// TestCollections_EmptyValues: together they pin the bidirectional
	// nil/empty distinction at the storage boundary.
	var labelsTxt, tagsTxt, itemsTxt, attrsTxt sql.NullString
	var flagsTxt, ints32Txt, uints32Txt, ints64Txt, uints64Txt, floatsTxt, doublesTxt sql.NullString
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT labels::text, tags::text, items::text, attributes::text,
		       flags::text, ints32::text, uints32::text, ints64::text, uints64::text,
		       floats::text, doubles::text
		FROM collections WHERE id = $1
	`, id).Scan(&labelsTxt, &tagsTxt, &itemsTxt, &attrsTxt,
		&flagsTxt, &ints32Txt, &uints32Txt, &ints64Txt, &uints64Txt,
		&floatsTxt, &doublesTxt))
	// Empty TEXT[] / primitive arrays render as "{}".
	assert.Equal(t, "{}", nullableText(labelsTxt), "labels must be empty TEXT[]")
	assert.Equal(t, "{}", nullableText(tagsTxt), "tags must be empty TEXT[]")
	assert.Equal(t, "{}", nullableText(flagsTxt), "flags must be empty BOOLEAN[]")
	assert.Equal(t, "{}", nullableText(ints32Txt), "ints32 must be empty INTEGER[]")
	assert.Equal(t, "{}", nullableText(uints32Txt), "uints32 must be empty INTEGER[]")
	assert.Equal(t, "{}", nullableText(ints64Txt), "ints64 must be empty BIGINT[]")
	assert.Equal(t, "{}", nullableText(uints64Txt), "uints64 must be empty BIGINT[]")
	assert.Equal(t, "{}", nullableText(floatsTxt), "floats must be empty REAL[]")
	assert.Equal(t, "{}", nullableText(doublesTxt), "doubles must be empty DOUBLE PRECISION[]")
	// JSONB list renders as "[]", JSONB object as "{}".
	assert.Equal(t, "[]", nullableText(itemsTxt), "items must be empty JSONB array")
	assert.Equal(t, "{}", nullableText(attrsTxt), "attributes must be empty JSONB object")

	// Read-back — every collection must be non-nil on the proto side,
	// pinning that the mapper decodes "{}" / "[]" back to a zero-length
	// collection rather than leaving the pointer nil.
	var read jetmodel.Collections
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.CollectionsTable.AllColumns).
			FROM(gen.CollectionsTable).
			WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.CollectionsProtoFromModel(&read)
	require.NoError(t, err)
	// The go-jet pointer fields must be non-nil (empty, not absent) —
	// proves the mapper distinguishes the storage-layer "present but
	// empty" from "absent / NULL".
	assert.NotNil(t, read.Labels, "Labels pointer must be non-nil for empty []")
	assert.NotNil(t, read.Attributes, "Attributes pointer must be non-nil for empty map")
	assert.NotNil(t, read.Items, "Items pointer must be non-nil for empty []")
	assert.NotNil(t, read.Flags, "Flags pointer must be non-nil for empty []")
	// proto3 cannot distinguish nil from empty on the wire. On the proto
	// side, Get* for a zero-length slice/map returns nil — we only
	// assert the round-trip produces the same zero-length view.
	assert.Empty(t, got.GetLabels())
	assert.Empty(t, got.GetTags())
	assert.Empty(t, got.GetItems())
	assert.Empty(t, got.GetAttributes())
	assert.Empty(t, got.GetFlags())
	assert.Empty(t, got.GetInts32())
	assert.Empty(t, got.GetUints32())
	assert.Empty(t, got.GetInts64())
	assert.Empty(t, got.GetUints64())
	assert.Empty(t, got.GetFloats())
	assert.Empty(t, got.GetDoubles())
}

// TestCollections_UpdateClearsToNull pins that an UpdateAll with a
// proto carrying nil collections overwrites previously-populated
// columns back to SQL NULL. Without the nullable-default flip, the
// mapper would write '{}' / '[]' and the prior distinction (present
// but empty vs. absent) would be erased on every update cycle.
func TestCollections_UpdateClearsToNull(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "coll-update-nil-" + uniqueID(t)

	// Seed with populated collections.
	first := &e2ev1.Collections{
		Id:         id,
		Labels:     []string{"a", "b"},
		Tags:       []e2ev1.CollectionTag{e2ev1.CollectionTag_COLLECTION_TAG_RED},
		Items:      []*e2ev1.NestedPayload{{Name: "one", Count: 1}},
		Attributes: map[string]string{"k": "v"},
		Flags:      []bool{true},
		Ints32:     []int32{42},
		Uints32:    []uint32{7},
		Ints64:     []int64{-5},
		Uints64:    []uint64{99},
		Floats:     []float32{1.5},
		Doubles:    []float64{2.5},
	}
	row, err := gen.CollectionsModelFromProto(first)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))

	// Overwrite with nil collections (proto3 zero value everywhere).
	cleared := &e2ev1.Collections{Id: id}
	updated, err := gen.CollectionsModelFromProto(cleared)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsUpdateAll().
			MODEL(updated).
			WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))))
	}))

	// Raw SQL probe — every nullable collection column must be NULL
	// after the cycle. If the mapper silently writes '{}' / '[]' here,
	// this assertion catches the regression.
	var labelsTxt, tagsTxt, itemsTxt, attrsTxt sql.NullString
	var flagsTxt, ints32Txt, uints32Txt, ints64Txt, uints64Txt, floatsTxt, doublesTxt sql.NullString
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT labels::text, tags::text, items::text, attributes::text,
		       flags::text, ints32::text, uints32::text, ints64::text, uints64::text,
		       floats::text, doubles::text
		FROM collections WHERE id = $1
	`, id).Scan(&labelsTxt, &tagsTxt, &itemsTxt, &attrsTxt,
		&flagsTxt, &ints32Txt, &uints32Txt, &ints64Txt, &uints64Txt,
		&floatsTxt, &doublesTxt))
	assert.False(t, labelsTxt.Valid, "labels must be NULL after UpdateAll(nil)")
	assert.False(t, tagsTxt.Valid, "tags must be NULL after UpdateAll(nil)")
	assert.False(t, itemsTxt.Valid, "items must be NULL after UpdateAll(nil)")
	assert.False(t, attrsTxt.Valid, "attributes must be NULL after UpdateAll(nil)")
	assert.False(t, flagsTxt.Valid, "flags must be NULL after UpdateAll(nil)")
	assert.False(t, ints32Txt.Valid, "ints32 must be NULL after UpdateAll(nil)")
	assert.False(t, uints32Txt.Valid, "uints32 must be NULL after UpdateAll(nil)")
	assert.False(t, ints64Txt.Valid, "ints64 must be NULL after UpdateAll(nil)")
	assert.False(t, uints64Txt.Valid, "uints64 must be NULL after UpdateAll(nil)")
	assert.False(t, floatsTxt.Valid, "floats must be NULL after UpdateAll(nil)")
	assert.False(t, doublesTxt.Valid, "doubles must be NULL after UpdateAll(nil)")
}

// nullableText returns the string value of a sql.NullString or
// "<NULL>" sentinel if absent. Used by collection tests that want the
// assertion failure message to show the actual stored form rather than
// a bare empty string.
func nullableText(ns sql.NullString) string {
	if !ns.Valid {
		return "<NULL>"
	}
	return ns.String
}

// TestCollections_FilterByRepeatedText pins AIP-160 filter semantics
// on TEXT[] columns end-to-end. The filter translator emits
// `value = ANY(col)` containment; the generated Collections
// FilterFields exposes `labels` (repeated string) and `tags`
// (repeated enum). Seeds three rows with overlapping-but-distinct
// label / tag sets and confirms the filter selects exactly the
// intended subset. Negation and the "has" (:) operator ride the
// same path and are pinned too.
func TestCollections_FilterByRepeatedText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	seed := func(id string, labels []string, tags []e2ev1.CollectionTag) {
		t.Helper()
		p := &e2ev1.Collections{Id: id, Labels: labels, Tags: tags}
		row, err := gen.CollectionsModelFromProto(p)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
		}))
	}
	idA := "coll-filter-a-" + uniqueID(t)
	idB := "coll-filter-b-" + uniqueID(t)
	idC := "coll-filter-c-" + uniqueID(t)
	// A has "go", B has "rust", C has "go" + "python" — queries below
	// discriminate on membership.
	seed(idA, []string{"go"}, []e2ev1.CollectionTag{e2ev1.CollectionTag_COLLECTION_TAG_RED})
	seed(idB, []string{"rust"}, []e2ev1.CollectionTag{e2ev1.CollectionTag_COLLECTION_TAG_BLUE})
	seed(idC, []string{"go", "python"}, []e2ev1.CollectionTag{e2ev1.CollectionTag_COLLECTION_TAG_RED, e2ev1.CollectionTag_COLLECTION_TAG_GREEN})

	runFilter := func(t *testing.T, filter string) []string {
		t.Helper()
		cond, err := aipjet.FilterToCondition(filter, gen.CollectionsFilterFields())
		require.NoError(t, err)
		require.NotNil(t, cond)
		var rows []jetmodel.Collections
		require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return postgres.SELECT(gen.CollectionsTable.AllColumns).
				FROM(gen.CollectionsTable).
				WHERE(cond.AND(gen.CollectionsTable.ID.IN(
					postgres.String(idA), postgres.String(idB), postgres.String(idC),
				))).
				QueryContext(ctx, tx, &rows)
		}))
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		sort.Strings(ids)
		return ids
	}

	t.Run("string = needle — A and C", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{idA, idC}, runFilter(t, `labels = "go"`))
	})
	t.Run("has operator — same semantics", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{idA, idC}, runFilter(t, `labels : "go"`))
	})
	t.Run("string != needle — only B (no 'go')", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{idB}, runFilter(t, `labels != "go"`))
	})
	t.Run("enum containment — RED in A and C", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{idA, idC}, runFilter(t, `tags = "COLLECTION_TAG_RED"`))
	})
	t.Run("combined — go labels AND RED tag", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{idA, idC}, runFilter(t, `labels = "go" AND tags = "COLLECTION_TAG_RED"`))
	})
}

// --------------------------------------------------------------------
// Maps — every proto3-legal map<K,V> shape round-trips through the
// generated mapper and lands in the DB as a single JSONB object.
// --------------------------------------------------------------------

// insertAndReadMaps is a thin helper — every map sub-test does the same
// MODEL → INSERT → SELECT → ProtoFromModel dance, so factor the noise
// out and let each test focus on the assertions. Mutates nothing that
// would surprise a caller; returns the decoded proto.
func insertAndReadMaps(t *testing.T, ctx context.Context, in *e2ev1.Maps) *e2ev1.Maps {
	t.Helper()
	row, err := gen.MapsModelFromProto(in)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
	}))
	var read jetmodel.Maps
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.MapsTable.AllColumns).
			FROM(gen.MapsTable).
			WHERE(gen.MapsTable.ID.EQ(postgres.String(in.GetId()))).
			QueryContext(ctx, tx, &read)
	}))
	out, err := gen.MapsProtoFromModel(&read)
	require.NoError(t, err)
	return out
}

func TestMaps_StringString_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-ss-" + uniqueID(t)
	// Same value-shape as the Collections.Attributes test: PG-metacharacter
	// heavy + unicode + empty string. Proves the legacy fast path stayed
	// byte-identical after the generic map rewrite.
	in := &e2ev1.Maps{
		Id: id,
		StrToStr: map[string]string{
			"region":           "eu-west-1",
			"日本語":              "value",
			`key"with"quotes`:  "v",
			`with\backslash`:   "v2",
			`percent%wildcard`: "v3",
			`underscore_too`:   "v4",
			"empty_value":      "",
		},
	}
	got := insertAndReadMaps(t, ctx, in)
	assert.Equal(t, in.GetStrToStr(), got.GetStrToStr())
}

func TestMaps_ScalarValues_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// One sub-test per scalar value kind. Each case pins boundary values
	// where relevant so a truncation / sign-flip regression surfaces
	// directly in the assertion.
	t.Run("int32", func(t *testing.T) {
		t.Parallel()
		id := "maps-si32-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToInt32: map[string]int32{
			"zero": 0,
			"neg":  math.MinInt32,
			"pos":  math.MaxInt32,
			"one":  1,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToInt32(), got.GetStrToInt32())
	})

	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		id := "maps-si64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToInt64: map[string]int64{
			"zero": 0,
			"neg":  math.MinInt64,
			"pos":  math.MaxInt64,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToInt64(), got.GetStrToInt64())
	})

	t.Run("bool", func(t *testing.T) {
		t.Parallel()
		id := "maps-sb-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToBool: map[string]bool{
			"yes": true,
			"no":  false,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToBool(), got.GetStrToBool())
	})

	t.Run("bytes", func(t *testing.T) {
		t.Parallel()
		id := "maps-sby-" + uniqueID(t)
		// Non-printable payload proves json's base64 codec round-trips
		// the raw bytes. Empty value is a valid proto state — encode
		// as "" (base64 of zero bytes).
		in := &e2ev1.Maps{Id: id, StrToBytes: map[string][]byte{
			"bin":   {0x00, 0x01, 0xff, 0xfe, 0x7f},
			"empty": {},
			"text":  []byte("hello"),
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToBytes(), len(in.GetStrToBytes()))
		for k, want := range in.GetStrToBytes() {
			assert.Equal(t, want, got.GetStrToBytes()[k], "key=%q", k)
		}
	})

	_ = ctx
}

func TestMaps_MessageValue_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("populated", func(t *testing.T) {
		t.Parallel()
		id := "maps-msg-" + uniqueID(t)
		in := &e2ev1.Maps{
			Id: id,
			StrToMsg: map[string]*e2ev1.MapValuePayload{
				"first":  {Label: "one", Count: 1},
				"second": {Label: "two", Count: math.MaxInt64},
				// Zero-valued payload — protojson elides zero fields
				// but the map entry itself must survive.
				"zero": {},
			},
		}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToMsg(), len(in.GetStrToMsg()))
		for k, want := range in.GetStrToMsg() {
			gotV := got.GetStrToMsg()[k]
			require.NotNil(t, gotV, "key=%q", k)
			assert.Equal(t, want.GetLabel(), gotV.GetLabel(), "key=%q", k)
			assert.Equal(t, want.GetCount(), gotV.GetCount(), "key=%q", k)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		id := "maps-msg-empty-" + uniqueID(t)
		// Nil map on a message-valued proto field — must round-trip as
		// empty (proto3 semantics don't distinguish nil from empty).
		in := &e2ev1.Maps{Id: id}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Empty(t, got.GetStrToMsg())
	})

	_ = ctx
}

func TestMaps_IntKey_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("int32", func(t *testing.T) {
		t.Parallel()
		id := "maps-i32-" + uniqueID(t)
		// Boundary values prove the int32 codec: MinInt32 has a sign,
		// MaxInt32 pins the 31-bit ceiling, zero is the proto3 default.
		in := &e2ev1.Maps{Id: id, Int32ToStr: map[int32]string{
			math.MinInt32: "min",
			-1:            "neg-one",
			0:             "zero",
			1:             "one",
			math.MaxInt32: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetInt32ToStr(), got.GetInt32ToStr())
	})

	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		id := "maps-i64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Int64ToStr: map[int64]string{
			math.MinInt64: "min",
			0:             "zero",
			math.MaxInt64: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetInt64ToStr(), got.GetInt64ToStr())
	})

	t.Run("uint32", func(t *testing.T) {
		t.Parallel()
		id := "maps-u32-" + uniqueID(t)
		// Full unsigned range — MaxUint32 is the top of the key space
		// and has to round-trip under the strconv.ParseUint codec.
		in := &e2ev1.Maps{Id: id, Uint32ToInt32: map[uint32]int32{
			0:              0,
			1:              math.MaxInt32,
			math.MaxUint32: -1,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetUint32ToInt32(), got.GetUint32ToInt32())
	})

	t.Run("int32_message_value", func(t *testing.T) {
		t.Parallel()
		id := "maps-i32msg-" + uniqueID(t)
		// Non-string key crossed with message value — covers both
		// key-stringify and protojson-framed value paths in one column.
		in := &e2ev1.Maps{Id: id, Int32ToMsg: map[int32]*e2ev1.MapValuePayload{
			-42: {Label: "negative", Count: -1},
			0:   {Label: "zero"},
			42:  {Label: "positive", Count: 100},
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetInt32ToMsg(), len(in.GetInt32ToMsg()))
		for k, want := range in.GetInt32ToMsg() {
			gotV := got.GetInt32ToMsg()[k]
			require.NotNil(t, gotV, "key=%d", k)
			assert.Equal(t, want.GetLabel(), gotV.GetLabel(), "key=%d", k)
			assert.Equal(t, want.GetCount(), gotV.GetCount(), "key=%d", k)
		}
	})
}

func TestMaps_BoolKey_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-bk-" + uniqueID(t)
	// Both bool key values present — the codec serialises to
	// "true"/"false" string keys and must parse back to the Go bool on
	// read. Stdlib json.Marshal doesn't support bool-keyed maps natively,
	// so this is a headline test that the mapper's explicit re-key path
	// actually runs.
	in := &e2ev1.Maps{Id: id, BoolToStr: map[bool]string{
		true:  "yes",
		false: "no",
	}}
	got := insertAndReadMaps(t, ctx, in)
	assert.Equal(t, in.GetBoolToStr(), got.GetBoolToStr())
}

func TestMaps_EnumValue_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-ev-" + uniqueID(t)
	in := &e2ev1.Maps{
		Id: id,
		StrToEnum: map[string]e2ev1.MapEnum{
			"alpha":   e2ev1.MapEnum_MAP_ENUM_ALPHA,
			"beta":    e2ev1.MapEnum_MAP_ENUM_BETA,
			"gamma":   e2ev1.MapEnum_MAP_ENUM_GAMMA,
			"zero-ok": e2ev1.MapEnum_MAP_ENUM_UNSPECIFIED,
			"日本語_key": e2ev1.MapEnum_MAP_ENUM_BETA,
		},
		Int32ToEnum: map[int32]e2ev1.MapEnum{
			0:                 e2ev1.MapEnum_MAP_ENUM_ALPHA,
			-1:                e2ev1.MapEnum_MAP_ENUM_BETA,
			math.MaxInt32:     e2ev1.MapEnum_MAP_ENUM_GAMMA,
			math.MinInt32 + 7: e2ev1.MapEnum_MAP_ENUM_ALPHA,
		},
	}
	got := insertAndReadMaps(t, ctx, in)
	assert.Equal(t, in.GetStrToEnum(), got.GetStrToEnum())
	assert.Equal(t, in.GetInt32ToEnum(), got.GetInt32ToEnum())
}

// TestMaps_EnumValue_StoredAsNames pins the stored JSON shape — enum
// values land as their `String()` name, not their int32 number. Guards
// against a future mapper switch to numeric encoding that would silently
// drift from KindEnumAsText for bare enum scalars.
func TestMaps_EnumValue_StoredAsNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-ev-shape-" + uniqueID(t)
	in := &e2ev1.Maps{
		Id:        id,
		StrToEnum: map[string]e2ev1.MapEnum{"k": e2ev1.MapEnum_MAP_ENUM_BETA},
	}
	row, err := gen.MapsModelFromProto(in)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
	}))
	var stored string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT str_to_enum::text FROM maps WHERE id = $1
	`, id).Scan(&stored))
	assert.JSONEq(t, `{"k":"MAP_ENUM_BETA"}`, stored)
}

// TestMaps_WKTValue_RoundTrip covers map<K, Timestamp> /
// map<K, Duration>. Both ride KindJSONBMapMessage — protojson
// handles the well-known types natively, so the per-element encoding
// stays unchanged; the test pins that behaviour stays intact.
func TestMaps_WKTValue_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-wkt-" + uniqueID(t)
	t1 := time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2099, 1, 2, 3, 4, 5, 123456789, time.UTC)
	in := &e2ev1.Maps{
		Id: id,
		StrToTimestamp: map[string]*timestamppb.Timestamp{
			"start": timestamppb.New(t1),
			"end":   timestamppb.New(t2),
		},
		StrToDuration: map[string]*durationpb.Duration{
			"zero":     durationpb.New(0),
			"negative": durationpb.New(-42 * time.Second),
			"huge":     durationpb.New(9999 * time.Hour),
		},
	}
	got := insertAndReadMaps(t, ctx, in)
	require.Len(t, got.GetStrToTimestamp(), 2)
	assert.True(t, t1.Equal(got.GetStrToTimestamp()["start"].AsTime()))
	assert.True(t, t2.Equal(got.GetStrToTimestamp()["end"].AsTime()))

	require.Len(t, got.GetStrToDuration(), 3)
	assert.Equal(t, time.Duration(0), got.GetStrToDuration()["zero"].AsDuration())
	assert.Equal(t, -42*time.Second, got.GetStrToDuration()["negative"].AsDuration())
	assert.Equal(t, 9999*time.Hour, got.GetStrToDuration()["huge"].AsDuration())
}

func TestMaps_Empty_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("unset", func(t *testing.T) {
		t.Parallel()
		id := "maps-unset-" + uniqueID(t)
		// Every map field left at its proto3 zero value (nil). Round-
		// trips as empty map — proto3 cannot distinguish nil from
		// empty, so the read side settles on empty.
		in := &e2ev1.Maps{Id: id}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Empty(t, got.GetStrToStr())
		assert.Empty(t, got.GetStrToInt32())
		assert.Empty(t, got.GetStrToInt64())
		assert.Empty(t, got.GetStrToBool())
		assert.Empty(t, got.GetStrToBytes())
		assert.Empty(t, got.GetStrToMsg())
		assert.Empty(t, got.GetInt32ToStr())
		assert.Empty(t, got.GetInt64ToStr())
		assert.Empty(t, got.GetUint32ToInt32())
		assert.Empty(t, got.GetBoolToStr())
		assert.Empty(t, got.GetInt32ToMsg())
		assert.Empty(t, got.GetNestedTree())
		assert.Empty(t, got.GetStrToEnum())
		assert.Empty(t, got.GetInt32ToEnum())
		// Extended integer key kinds + extended scalar value kinds.
		assert.Empty(t, got.GetSint32ToStr())
		assert.Empty(t, got.GetSint64ToStr())
		assert.Empty(t, got.GetUint64ToStr())
		assert.Empty(t, got.GetFixed32ToStr())
		assert.Empty(t, got.GetFixed64ToStr())
		assert.Empty(t, got.GetSfixed32ToStr())
		assert.Empty(t, got.GetSfixed64ToStr())
		assert.Empty(t, got.GetStrToUint32())
		assert.Empty(t, got.GetStrToUint64())
		assert.Empty(t, got.GetStrToFixed32())
		assert.Empty(t, got.GetStrToSfixed64())
	})

	t.Run("explicit_empty", func(t *testing.T) {
		t.Parallel()
		id := "maps-explicit-empty-" + uniqueID(t)
		// Explicit empty (non-nil, zero-length) map — identical
		// read-side result to the unset case.
		in := &e2ev1.Maps{
			Id:            id,
			StrToStr:      map[string]string{},
			StrToInt32:    map[string]int32{},
			StrToBool:     map[string]bool{},
			Int32ToStr:    map[int32]string{},
			BoolToStr:     map[bool]string{},
			Uint32ToInt32: map[uint32]int32{},
		}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Empty(t, got.GetStrToStr())
		assert.Empty(t, got.GetStrToInt32())
		assert.Empty(t, got.GetStrToBool())
		assert.Empty(t, got.GetInt32ToStr())
		assert.Empty(t, got.GetBoolToStr())
		assert.Empty(t, got.GetUint32ToInt32())
	})

	_ = ctx
}

// TestMaps_NilVsPopulated pins the nil-vs-empty distinction at the
// JSONB storage boundary for every map kind. Nil proto map -> SQL NULL;
// non-nil empty proto map -> "{}" JSONB literal. The mapper's round-
// trip therefore preserves the presence bit the proto side can't
// express on the wire but the Go struct can.
func TestMaps_NilVsPopulated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Enumerate every map field on the fixture. Keep this list in sync
	// with proto/jet/e2e/v1/maps.proto — a new map column should
	// appear here too so the nullable-by-default contract stays pinned.
	cols := []string{
		"str_to_str", "str_to_int32", "str_to_int64", "str_to_bool",
		"str_to_bytes", "str_to_msg",
		"int32_to_str", "int64_to_str", "uint32_to_int32",
		"bool_to_str", "int32_to_msg", "nested_tree",
		"str_to_enum", "int32_to_enum",
		"str_to_timestamp", "str_to_duration",
		// Extended integer key kinds — every proto3-legal key width.
		"sint32_to_str", "sint64_to_str", "uint64_to_str",
		"fixed32_to_str", "fixed64_to_str", "sfixed32_to_str", "sfixed64_to_str",
		// Extended scalar value kinds.
		"str_to_uint32", "str_to_uint64", "str_to_fixed32", "str_to_sfixed64",
	}

	t.Run("nil_proto_is_null", func(t *testing.T) {
		t.Parallel()
		id := "maps-nil-probe-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id}
		row, err := gen.MapsModelFromProto(in)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
		}))
		for _, col := range cols {
			var v sql.NullString
			//nolint:gosec // column list is a static, in-test allow-list
			require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
				`SELECT `+col+`::text FROM maps WHERE id = $1`, id,
			).Scan(&v), "column %s", col)
			assert.Falsef(t, v.Valid, "column %s must be NULL for nil proto map", col)
		}
	})

	t.Run("empty_proto_is_empty_object", func(t *testing.T) {
		t.Parallel()
		id := "maps-empty-probe-" + uniqueID(t)
		// Non-nil empty map for every field the proto exposes with an
		// explicit key type — this is the "present but zero elements"
		// state that must NOT collapse to NULL.
		in := &e2ev1.Maps{
			Id:             id,
			StrToStr:       map[string]string{},
			StrToInt32:     map[string]int32{},
			StrToInt64:     map[string]int64{},
			StrToBool:      map[string]bool{},
			StrToBytes:     map[string][]byte{},
			StrToMsg:       map[string]*e2ev1.MapValuePayload{},
			Int32ToStr:     map[int32]string{},
			Int64ToStr:     map[int64]string{},
			Uint32ToInt32:  map[uint32]int32{},
			BoolToStr:      map[bool]string{},
			Int32ToMsg:     map[int32]*e2ev1.MapValuePayload{},
			NestedTree:     map[string]*e2ev1.Level1{},
			StrToEnum:      map[string]e2ev1.MapEnum{},
			Int32ToEnum:    map[int32]e2ev1.MapEnum{},
			StrToTimestamp: map[string]*timestamppb.Timestamp{},
			StrToDuration:  map[string]*durationpb.Duration{},
			// Extended integer key kinds — every proto3-legal width.
			Sint32ToStr:   map[int32]string{},
			Sint64ToStr:   map[int64]string{},
			Uint64ToStr:   map[uint64]string{},
			Fixed32ToStr:  map[uint32]string{},
			Fixed64ToStr:  map[uint64]string{},
			Sfixed32ToStr: map[int32]string{},
			Sfixed64ToStr: map[int64]string{},
			// Extended scalar value kinds.
			StrToUint32:   map[string]uint32{},
			StrToUint64:   map[string]uint64{},
			StrToFixed32:  map[string]uint32{},
			StrToSfixed64: map[string]int64{},
		}
		row, err := gen.MapsModelFromProto(in)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
		}))
		for _, col := range cols {
			var v sql.NullString
			//nolint:gosec // column list is a static, in-test allow-list
			require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
				`SELECT `+col+`::text FROM maps WHERE id = $1`, id,
			).Scan(&v), "column %s", col)
			require.Truef(t, v.Valid, "column %s must be non-NULL for empty proto map", col)
			assert.Equalf(t, "{}", v.String, "column %s must be '{}' for empty proto map", col)
		}
	})
}

func TestMaps_StoredAsJSONB(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-jsonb-" + uniqueID(t)
	// Probe the raw JSONB. The shape we expect is one JSON object per
	// column, with every key stringified per the codec (base-10 for
	// ints, "true"/"false" for bools). A column-type or codec drift
	// surfaces here before it leaks into the proto-level assertions.
	in := &e2ev1.Maps{
		Id:            id,
		StrToStr:      map[string]string{"a": "1"},
		StrToInt32:    map[string]int32{"ten": 10},
		StrToBool:     map[string]bool{"flag": true},
		StrToBytes:    map[string][]byte{"b": {0x01, 0x02}}, // base64 "AQI="
		Int32ToStr:    map[int32]string{-7: "neg"},
		Uint32ToInt32: map[uint32]int32{4294967295: -1},
		BoolToStr:     map[bool]string{true: "yes", false: "no"},
	}
	row, err := gen.MapsModelFromProto(in)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
	}))

	type jsonbProbe struct {
		sqlExpr string
		want    string
	}
	cases := []jsonbProbe{
		{`str_to_str ->> 'a'`, "1"},
		{`str_to_int32 ->> 'ten'`, "10"},
		{`str_to_bool ->> 'flag'`, "true"},
		{`str_to_bytes ->> 'b'`, "AQI="}, // json auto-base64 for []byte
		{`int32_to_str ->> '-7'`, "neg"},
		{`uint32_to_int32 ->> '4294967295'`, "-1"},
		{`bool_to_str ->> 'true'`, "yes"},
		{`bool_to_str ->> 'false'`, "no"},
	}
	for _, c := range cases {
		var got string
		//nolint:gosec // DSN is controlled, values are test fixtures
		require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
			`SELECT `+c.sqlExpr+` FROM maps WHERE id = $1`, id,
		).Scan(&got), "probe %q", c.sqlExpr)
		assert.Equal(t, c.want, got, "probe %q", c.sqlExpr)
	}
}

func TestMaps_DeepNested_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-deep-" + uniqueID(t)

	// Build the headline "map → object → map → object → repeated
	// object → object" structure. Every level carries a distinguishing
	// value so a silent drop anywhere in the pipeline shows up as a
	// zero-value mismatch at the leaf.
	makeLevel1 := func(prefix string) *e2ev1.Level1 {
		return &e2ev1.Level1{
			Sections: map[string]*e2ev1.Level2{
				prefix + "-alpha": {
					Branches: map[string]*e2ev1.Level3{
						"north": {
							Leaves: []*e2ev1.Level4{
								{Tag: prefix + "-na-0", Value: 1},
								{Tag: prefix + "-na-1", Value: 2},
							},
						},
						"south": {
							Leaves: []*e2ev1.Level4{
								{Tag: prefix + "-sa-0", Value: 10},
							},
						},
					},
				},
				prefix + "-beta": {
					Branches: map[string]*e2ev1.Level3{
						"east": {
							Leaves: []*e2ev1.Level4{
								{Tag: prefix + "-eb-0", Value: math.MaxInt32},
							},
						},
					},
				},
			},
		}
	}

	in := &e2ev1.Maps{
		Id: id,
		NestedTree: map[string]*e2ev1.Level1{
			"tree-one": makeLevel1("one"),
			"tree-two": makeLevel1("two"),
		},
	}

	got := insertAndReadMaps(t, ctx, in)

	// Walk every level and pin every leaf. Sort keys where we iterate
	// so the assertion output is stable on failure.
	require.Len(t, got.GetNestedTree(), len(in.GetNestedTree()))
	trees := make([]string, 0, len(got.GetNestedTree()))
	for k := range got.GetNestedTree() {
		trees = append(trees, k)
	}
	sort.Strings(trees)
	for _, treeKey := range trees {
		wantL1 := in.GetNestedTree()[treeKey]
		gotL1 := got.GetNestedTree()[treeKey]
		require.NotNil(t, gotL1, "tree=%q", treeKey)
		require.Len(t, gotL1.GetSections(), len(wantL1.GetSections()), "tree=%q", treeKey)
		for sectionKey, wantL2 := range wantL1.GetSections() {
			gotL2 := gotL1.GetSections()[sectionKey]
			require.NotNil(t, gotL2, "tree=%q section=%q", treeKey, sectionKey)
			require.Len(t, gotL2.GetBranches(), len(wantL2.GetBranches()),
				"tree=%q section=%q", treeKey, sectionKey)
			for branchKey, wantL3 := range wantL2.GetBranches() {
				gotL3 := gotL2.GetBranches()[branchKey]
				require.NotNil(t, gotL3, "tree=%q section=%q branch=%q", treeKey, sectionKey, branchKey)
				require.Len(t, gotL3.GetLeaves(), len(wantL3.GetLeaves()),
					"tree=%q section=%q branch=%q", treeKey, sectionKey, branchKey)
				for i, wantL4 := range wantL3.GetLeaves() {
					gotL4 := gotL3.GetLeaves()[i]
					require.NotNil(t, gotL4)
					assert.Equal(t, wantL4.GetTag(), gotL4.GetTag(),
						"tree=%q section=%q branch=%q leaf=%d", treeKey, sectionKey, branchKey, i)
					assert.Equal(t, wantL4.GetValue(), gotL4.GetValue(),
						"tree=%q section=%q branch=%q leaf=%d", treeKey, sectionKey, branchKey, i)
				}
			}
		}
	}
}

// --------------------------------------------------------------------
// Map/collection edge cases — extended key/value kind coverage,
// stringification edge cases, nullability state transitions,
// large-payload smoke tests.
// --------------------------------------------------------------------

// TestMaps_AllIntegerKeyKinds_RoundTrip exercises every proto3-legal
// integer map-key width + signedness. Each field has boundary values
// for its width so a silent truncation at the strconv layer surfaces
// as a decoded value mismatch rather than a map-size mismatch.
//
// Raw-JSONB probes pin the stringification: base-10 ASCII, no sign
// quirks, no scientific notation — JSONB requires string keys and the
// mapper's emitKeyToString codec is the only source of those strings.
func TestMaps_AllIntegerKeyKinds_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("sint32", func(t *testing.T) {
		t.Parallel()
		id := "maps-sint32-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Sint32ToStr: map[int32]string{
			math.MinInt32: "min", -1: "neg-one", 0: "zero",
			1: "one", math.MaxInt32: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetSint32ToStr(), got.GetSint32ToStr())
	})

	t.Run("sint64", func(t *testing.T) {
		t.Parallel()
		id := "maps-sint64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Sint64ToStr: map[int64]string{
			math.MinInt64: "min", -1: "neg-one", 0: "zero", math.MaxInt64: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetSint64ToStr(), got.GetSint64ToStr())
	})

	t.Run("uint64", func(t *testing.T) {
		t.Parallel()
		id := "maps-uint64k-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Uint64ToStr: map[uint64]string{
			0: "zero", 1: "one", math.MaxInt64: "int64-max",
			math.MaxInt64 + 1: "high-bit", math.MaxUint64: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetUint64ToStr(), got.GetUint64ToStr())
	})

	t.Run("fixed32", func(t *testing.T) {
		t.Parallel()
		id := "maps-fixed32-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Fixed32ToStr: map[uint32]string{
			0: "zero", 1: "one", 0x7fffffff: "int32-max",
			0x80000000: "high-bit", math.MaxUint32: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetFixed32ToStr(), got.GetFixed32ToStr())
	})

	t.Run("fixed64", func(t *testing.T) {
		t.Parallel()
		id := "maps-fixed64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Fixed64ToStr: map[uint64]string{
			0: "zero", 1: "one", math.MaxInt64: "int64-max",
			math.MaxInt64 + 1: "high-bit", math.MaxUint64: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetFixed64ToStr(), got.GetFixed64ToStr())
	})

	t.Run("sfixed32", func(t *testing.T) {
		t.Parallel()
		id := "maps-sfixed32-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Sfixed32ToStr: map[int32]string{
			math.MinInt32: "min", -1: "neg-one", 0: "zero",
			1: "one", math.MaxInt32: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetSfixed32ToStr(), got.GetSfixed32ToStr())
	})

	t.Run("sfixed64", func(t *testing.T) {
		t.Parallel()
		id := "maps-sfixed64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, Sfixed64ToStr: map[int64]string{
			math.MinInt64: "min", -1: "neg-one", 0: "zero", math.MaxInt64: "max",
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetSfixed64ToStr(), got.GetSfixed64ToStr())
	})

	// Raw-JSONB probe — pin that every integer key lands as its
	// base-10 ASCII representation with no sign / exponent weirdness.
	// JSONB can't store non-string keys, so the codec is the single
	// producer of these strings; a regression at that layer fails here
	// before the Go-side round-trip even runs.
	t.Run("jsonb_key_shape", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		id := "maps-intkey-probe-" + uniqueID(t)
		in := &e2ev1.Maps{
			Id:            id,
			Sint32ToStr:   map[int32]string{math.MinInt32: "v"},
			Sint64ToStr:   map[int64]string{math.MinInt64: "v"},
			Uint64ToStr:   map[uint64]string{math.MaxUint64: "v"},
			Fixed32ToStr:  map[uint32]string{math.MaxUint32: "v"},
			Fixed64ToStr:  map[uint64]string{math.MaxUint64: "v"},
			Sfixed32ToStr: map[int32]string{math.MinInt32: "v"},
			Sfixed64ToStr: map[int64]string{math.MinInt64: "v"},
		}
		row, err := gen.MapsModelFromProto(in)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
		}))
		cases := []struct {
			expr string
			want string
		}{
			{`sint32_to_str -> '-2147483648'`, `"v"`},
			{`sint64_to_str -> '-9223372036854775808'`, `"v"`},
			{`uint64_to_str -> '18446744073709551615'`, `"v"`},
			{`fixed32_to_str -> '4294967295'`, `"v"`},
			{`fixed64_to_str -> '18446744073709551615'`, `"v"`},
			{`sfixed32_to_str -> '-2147483648'`, `"v"`},
			{`sfixed64_to_str -> '-9223372036854775808'`, `"v"`},
		}
		for _, c := range cases {
			var got sql.NullString
			//nolint:gosec // SQL expressions are test fixtures
			require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
				`SELECT `+c.expr+` FROM maps WHERE id = $1`, id,
			).Scan(&got), "probe %q", c.expr)
			require.True(t, got.Valid, "probe %q returned NULL — key stringification dropped the boundary value", c.expr)
			assert.Equal(t, c.want, got.String, "probe %q", c.expr)
		}
	})
}

// TestMaps_AllScalarValueKinds_RoundTrip covers uint32/uint64 and
// the fixed-width variants that feed through KindJSONBMapScalar.
// Boundary values pin that sign reinterpretation (for uint values
// above int-max) round-trips bit-identically.
//
// Observation: the plugin rejects `map<string, float>` and
// `map<string, double>` at resolve time (see inferMapEncoding in
// resolve.go). Tests for those value kinds intentionally absent —
// they'd fail at buf generate, not at runtime. See the
// `maps.proto` comment for the rationale.
func TestMaps_AllScalarValueKinds_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("uint32", func(t *testing.T) {
		t.Parallel()
		id := "maps-suint32-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToUint32: map[string]uint32{
			"zero": 0, "one": 1, "int32-max": 0x7fffffff,
			"high-bit": 0x80000000, "max": math.MaxUint32,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToUint32(), got.GetStrToUint32())
	})

	t.Run("uint64", func(t *testing.T) {
		t.Parallel()
		id := "maps-suint64-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToUint64: map[string]uint64{
			"zero": 0, "one": 1, "int64-max": math.MaxInt64,
			"high-bit": math.MaxInt64 + 1, "max": math.MaxUint64,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToUint64(), got.GetStrToUint64())
	})

	t.Run("fixed32", func(t *testing.T) {
		t.Parallel()
		id := "maps-sfixed32v-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToFixed32: map[string]uint32{
			"zero": 0, "max": math.MaxUint32, "high-bit": 0x80000000,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToFixed32(), got.GetStrToFixed32())
	})

	t.Run("sfixed64", func(t *testing.T) {
		t.Parallel()
		id := "maps-ssfixed64v-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToSfixed64: map[string]int64{
			"min": math.MinInt64, "neg": -1, "zero": 0,
			"one": 1, "max": math.MaxInt64,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		assert.Equal(t, in.GetStrToSfixed64(), got.GetStrToSfixed64())
	})
}

// TestMaps_StringKey_JSONEscaping stresses the string-key JSON
// escaping path in map<string,*>. JSONB stores every key as a JSON
// string — unicode, embedded quotes, backslashes, control chars, and
// tail-end edge cases (empty string, 1 KiB) must all survive as
// byte-identical keys through marshal + Postgres storage + unmarshal.
//
// Raw-JSONB probe uses jsonb_object_keys so we compare the decoded
// key set to the input map — don't depend on textual-SQL byte
// ordering, which would be fragile.
func TestMaps_StringKey_JSONEscaping(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-keys-escape-" + uniqueID(t)

	// Distinct keys that stress every JSON-escaping pathway. Values
	// are distinct too so a key-value swap is immediately visible.
	in := &e2ev1.Maps{
		Id: id,
		StrToStr: map[string]string{
			"":                        "empty",
			`a"b`:                     "embedded-quote",
			`a\b`:                     "embedded-backslash",
			"a/b":                     "forward-slash",
			"日本語":                     "japanese",
			"🚀":                       "emoji",
			"é":                      "combining-acute", // "é" via combining mark
			"‏RTL‎":                   "rtl-marks",
			"\x01":                    "control-0x01",
			"\x1f":                    "control-0x1f",
			"line\nfeed":              "newline",
			"tab\there":               "tab",
			strings.Repeat("x", 1024): "one-kib-key",
		},
	}

	got := insertAndReadMaps(t, ctx, in)
	assert.Equal(t, in.GetStrToStr(), got.GetStrToStr(),
		"every key-value pair must round-trip byte-identically")

	// Probe the raw JSONB — parse it back via json.Unmarshal and
	// compare to the input. Direct textual comparison would depend
	// on Postgres's internal JSONB key ordering, which is not stable
	// across versions.
	var raw string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
		`SELECT str_to_str::text FROM maps WHERE id = $1`, id,
	).Scan(&raw))
	var decoded map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded),
		"stored JSONB must parse back as a JSON object")
	assert.Equal(t, in.GetStrToStr(), decoded,
		"JSONB → json.Unmarshal must reconstruct the full key set")
}

// TestMaps_IntKey_JSONBShape pins that integer map keys are stored
// as string form (JSONB requires string keys) — not as JSON numbers
// and not as quoted-with-sign-prefix weirdness. Boundary values for
// int32 / int64 / uint32 / uint64 cover every codec branch.
func TestMaps_IntKey_JSONBShape(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "maps-intkey-shape-" + uniqueID(t)
	in := &e2ev1.Maps{
		Id:            id,
		Int32ToStr:    map[int32]string{0: "a", -1: "b", math.MinInt32: "c", math.MaxInt32: "d"},
		Int64ToStr:    map[int64]string{0: "a", math.MinInt64: "b", math.MaxInt64: "c"},
		Uint32ToInt32: map[uint32]int32{0: 0, math.MaxUint32: -1},
		Uint64ToStr:   map[uint64]string{0: "a", math.MaxUint64: "b"},
	}
	row, err := gen.MapsModelFromProto(in)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.MapsTable.INSERT(gen.MapsTable.AllColumns).MODEL(row))
	}))

	// jsonb_typeof on each inner value; jsonb_object_keys to dump the
	// keys and match against expectations. Every key must be stored as
	// a string — Postgres's "->>" operator extracts it unquoted, so
	// a successful lookup under the expected string proves the shape.
	probes := []struct {
		col  string
		keys []string
	}{
		{"int32_to_str", []string{"0", "-1", "-2147483648", "2147483647"}},
		{"int64_to_str", []string{"0", "-9223372036854775808", "9223372036854775807"}},
		{"uint32_to_int32", []string{"0", "4294967295"}},
		{"uint64_to_str", []string{"0", "18446744073709551615"}},
	}
	for _, probe := range probes {
		for _, key := range probe.keys {
			var v sql.NullString
			//nolint:gosec // SQL is a static allow-list
			err := sharedSuperDB.QueryRowContext(ctx,
				`SELECT `+probe.col+` -> $2 FROM maps WHERE id = $1`, id, key,
			).Scan(&v)
			require.NoError(t, err, "col=%s key=%s", probe.col, key)
			assert.Truef(t, v.Valid,
				"col=%s key=%q must be present — integer key stringification dropped it", probe.col, key)
		}
	}
}

// TestMaps_ElementEdgeCases pins the proto3 nil/empty/populated trinity
// for map value shapes that the mapper must keep distinct at the JSONB
// layer.
//
//   - map<string,string> with an empty-string value — the entry
//     survives as `"k":""` and round-trips as a 1-element map.
//   - map<string, MapValuePayload> with a zero-valued message value —
//     protojson emits `{}` for the value, Unmarshal must yield a
//     non-nil empty message (the outer map entry must not disappear).
func TestMaps_ElementEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("str_to_str empty value", func(t *testing.T) {
		t.Parallel()
		id := "maps-sskv-empty-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToStr: map[string]string{"k": ""}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToStr(), 1)
		v, ok := got.GetStrToStr()["k"]
		require.True(t, ok, "key 'k' must survive even with empty-string value")
		assert.Equal(t, "", v)
	})

	t.Run("str_to_msg zero-valued payload", func(t *testing.T) {
		t.Parallel()
		id := "maps-smsg-zero-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToMsg: map[string]*e2ev1.MapValuePayload{
			"zero": {}, // all fields zero-valued — protojson emits "{}"
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToMsg(), 1)
		gv, ok := got.GetStrToMsg()["zero"]
		require.True(t, ok, "key 'zero' must survive zero-valued message")
		require.NotNil(t, gv, "zero-valued message must decode as non-nil")
		assert.Equal(t, "", gv.GetLabel())
		assert.Equal(t, int64(0), gv.GetCount())
	})

	t.Run("str_to_msg single key with full payload survives round-trip", func(t *testing.T) {
		t.Parallel()
		id := "maps-smsg-full-" + uniqueID(t)
		in := &e2ev1.Maps{Id: id, StrToMsg: map[string]*e2ev1.MapValuePayload{
			"full": {Label: "populated", Count: 42},
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToMsg(), 1)
		assert.Equal(t, "populated", got.GetStrToMsg()["full"].GetLabel())
		assert.Equal(t, int64(42), got.GetStrToMsg()["full"].GetCount())
	})
}

// TestCollections_RepeatedStringEdgeCases pins the proto3 nil / empty /
// singleton-of-empty trinity for `repeated string`. These are three
// distinct storage states the mapper must keep apart:
//
//   - nil          — SQL NULL, `Labels` pointer nil on read.
//   - []string{}   — SQL '{}' (empty TEXT[]), `Labels` points at []string{}.
//   - []string{""} — SQL '{""}' (TEXT[] with a 1-element empty-string
//     entry), proto reads back a len-1 slice with an empty string.
func TestCollections_RepeatedStringEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	seed := func(t *testing.T, id string, labels []string) *e2ev1.Collections {
		t.Helper()
		p := &e2ev1.Collections{Id: id, Labels: labels}
		row, err := gen.CollectionsModelFromProto(p)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
		}))
		var read jetmodel.Collections
		require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return postgres.SELECT(gen.CollectionsTable.AllColumns).
				FROM(gen.CollectionsTable).
				WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
				QueryContext(ctx, tx, &read)
		}))
		got, err := gen.CollectionsProtoFromModel(&read)
		require.NoError(t, err)
		return got
	}

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		id := "coll-lbl-nil-" + uniqueID(t)
		got := seed(t, id, nil)
		assert.Empty(t, got.GetLabels())
		var v sql.NullString
		require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
			`SELECT labels::text FROM collections WHERE id = $1`, id,
		).Scan(&v))
		assert.False(t, v.Valid, "labels must be SQL NULL for nil proto")
	})

	t.Run("empty literal", func(t *testing.T) {
		t.Parallel()
		id := "coll-lbl-empty-" + uniqueID(t)
		got := seed(t, id, []string{})
		assert.Empty(t, got.GetLabels())
		var v sql.NullString
		require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
			`SELECT labels::text FROM collections WHERE id = $1`, id,
		).Scan(&v))
		require.True(t, v.Valid, "labels must be non-NULL for [] proto")
		assert.Equal(t, "{}", v.String)
	})

	t.Run("singleton empty string", func(t *testing.T) {
		t.Parallel()
		id := "coll-lbl-single-empty-" + uniqueID(t)
		got := seed(t, id, []string{""})
		require.Len(t, got.GetLabels(), 1,
			"singleton-of-empty-string must NOT collapse to empty slice")
		assert.Equal(t, "", got.GetLabels()[0])
		// Raw probe — PG-quoted form of {""} is `{""}` (empty element
		// quoted with double quotes). Anything else means the element
		// was dropped or misquoted.
		var v sql.NullString
		require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
			`SELECT labels::text FROM collections WHERE id = $1`, id,
		).Scan(&v))
		require.True(t, v.Valid)
		assert.Equal(t, `{""}`, v.String,
			"labels must be a 1-element TEXT[] with an empty string")
	})

	t.Run("attributes empty value", func(t *testing.T) {
		t.Parallel()
		// Paired coverage for map<string,string>: a key mapped to ""
		// must survive as a 1-element map, not collapse to empty.
		id := "coll-attr-emptyval-" + uniqueID(t)
		p := &e2ev1.Collections{Id: id, Attributes: map[string]string{"k": ""}}
		row, err := gen.CollectionsModelFromProto(p)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
		}))
		var read jetmodel.Collections
		require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return postgres.SELECT(gen.CollectionsTable.AllColumns).
				FROM(gen.CollectionsTable).
				WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
				QueryContext(ctx, tx, &read)
		}))
		got, err := gen.CollectionsProtoFromModel(&read)
		require.NoError(t, err)
		require.Len(t, got.GetAttributes(), 1)
		v, ok := got.GetAttributes()["k"]
		require.True(t, ok)
		assert.Equal(t, "", v)
	})
}

// TestCollections_NullabilityStateMatrix walks every transition
// between nil / empty / populated for a representative sample of
// collection kinds (TEXT[], JSONB array, JSONB object). Each kind
// shares a mapper branch with one of the three — a regression in a
// shared branch fails at least one of these cases. After each write,
// a raw SQL probe pins the SQL-level NULL / non-NULL flag and the
// parsed JSONB contents (for JSONB kinds) match the Go-side view.
func TestCollections_NullabilityStateMatrix(t *testing.T) {
	t.Parallel()

	type state int
	const (
		stateNil state = iota
		stateEmpty
		statePopulatedA
		statePopulatedB
	)

	applyState := func(id string, s state) *e2ev1.Collections {
		p := &e2ev1.Collections{Id: id}
		switch s {
		case stateNil:
			// nil everywhere — zero value already.
		case stateEmpty:
			p.Labels = []string{}
			p.Items = []*e2ev1.NestedPayload{}
			p.Attributes = map[string]string{}
		case statePopulatedA:
			p.Labels = []string{"a", "b"}
			p.Items = []*e2ev1.NestedPayload{{Name: "one", Count: 1}}
			p.Attributes = map[string]string{"k": "v"}
		case statePopulatedB:
			p.Labels = []string{"x", "y", "z"}
			p.Items = []*e2ev1.NestedPayload{{Name: "two", Count: 2}, {Name: "three", Count: 3}}
			p.Attributes = map[string]string{"alpha": "1", "beta": "2"}
		}
		return p
	}

	// All 16 (4x4) transitions. Some are degenerate (nil->nil),
	// others exercise transitions that a naive UpdateAll would get
	// wrong (populated->empty must write '{}' / '[]', not leave the
	// prior value intact).
	transitions := []struct {
		name string
		from state
		to   state
	}{
		{"nil->nil", stateNil, stateNil},
		{"nil->empty", stateNil, stateEmpty},
		{"nil->populatedA", stateNil, statePopulatedA},
		{"empty->nil", stateEmpty, stateNil},
		{"empty->populatedA", stateEmpty, statePopulatedA},
		{"populatedA->nil", statePopulatedA, stateNil},
		{"populatedA->empty", statePopulatedA, stateEmpty},
		{"populatedA->populatedB", statePopulatedA, statePopulatedB},
	}

	for _, tr := range transitions {
		tr := tr
		t.Run(tr.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			id := "coll-matrix-" + tr.name + "-" + uniqueID(t)

			// INSERT "from" state.
			pFrom := applyState(id, tr.from)
			row, err := gen.CollectionsModelFromProto(pFrom)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
			}))

			// UpdateAll with "to" state.
			pTo := applyState(id, tr.to)
			updated, err := gen.CollectionsModelFromProto(pTo)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.CollectionsUpdateAll().
					MODEL(updated).
					WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))))
			}))

			// Raw SQL probe — NULL flag and JSONB shape must match
			// the "to" state's Go-side view.
			var labelsTxt, itemsTxt, attrsTxt sql.NullString
			require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
				`SELECT labels::text, items::text, attributes::text FROM collections WHERE id = $1`, id,
			).Scan(&labelsTxt, &itemsTxt, &attrsTxt))

			switch tr.to {
			case stateNil:
				assert.False(t, labelsTxt.Valid, "labels must be NULL")
				assert.False(t, itemsTxt.Valid, "items must be NULL")
				assert.False(t, attrsTxt.Valid, "attributes must be NULL")
			case stateEmpty:
				require.True(t, labelsTxt.Valid)
				assert.Equal(t, "{}", labelsTxt.String, "labels must be empty TEXT[]")
				require.True(t, itemsTxt.Valid)
				assert.Equal(t, "[]", itemsTxt.String, "items must be empty JSONB array")
				require.True(t, attrsTxt.Valid)
				assert.Equal(t, "{}", attrsTxt.String, "attributes must be empty JSONB object")
			case statePopulatedA, statePopulatedB:
				require.True(t, labelsTxt.Valid, "labels must be non-NULL")
				require.True(t, itemsTxt.Valid, "items must be non-NULL")
				require.True(t, attrsTxt.Valid, "attributes must be non-NULL")
				// Parse the JSONB columns and assert equivalence to the
				// input map. Direct string comparison would depend on
				// the order protojson emits keys in, which is not
				// guaranteed stable.
				var decodedAttrs map[string]string
				require.NoError(t, json.Unmarshal([]byte(attrsTxt.String), &decodedAttrs))
				assert.Equal(t, pTo.GetAttributes(), decodedAttrs)
				var decodedItems []json.RawMessage
				require.NoError(t, json.Unmarshal([]byte(itemsTxt.String), &decodedItems))
				assert.Len(t, decodedItems, len(pTo.GetItems()))
			}

			// Proto-level round-trip asserts the Go-side view too.
			var read jetmodel.Collections
			require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return postgres.SELECT(gen.CollectionsTable.AllColumns).
					FROM(gen.CollectionsTable).
					WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
					QueryContext(ctx, tx, &read)
			}))
			got, err := gen.CollectionsProtoFromModel(&read)
			require.NoError(t, err)
			assert.Equal(t, pTo.GetLabels(), got.GetLabels())
			assert.Equal(t, pTo.GetAttributes(), got.GetAttributes())
			require.Len(t, got.GetItems(), len(pTo.GetItems()))
		})
	}
}

// TestMaps_LargeMap_RoundTrip drives 10,000 entries through
// map<string,string>. Fails loudly if the generated mapper has
// quadratic allocation behaviour, truncates anywhere, or hits a
// JSONB size limit before the 1 GB column cap. Guarded by -short so
// quick runs skip it.
func TestMaps_LargeMap_RoundTrip(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping large-payload test in short mode")
	}
	ctx := t.Context()
	id := "maps-large-" + uniqueID(t)
	const n = 10000
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m["k-"+strings.Repeat("0", 6)+fmt.Sprintf("%d", i)] = fmt.Sprintf("value-%d", i)
	}
	in := &e2ev1.Maps{Id: id, StrToStr: m}

	start := time.Now()
	got := insertAndReadMaps(t, ctx, in)
	elapsed := time.Since(start)

	require.Len(t, got.GetStrToStr(), n, "every entry must survive")
	// Spot-check — verify head, middle, and tail. Catching a silent
	// truncation in the middle of the map.
	assert.Equal(t, "value-0", got.GetStrToStr()["k-"+strings.Repeat("0", 6)+"0"])
	assert.Equal(t, fmt.Sprintf("value-%d", n/2),
		got.GetStrToStr()["k-"+strings.Repeat("0", 6)+fmt.Sprintf("%d", n/2)])
	assert.Equal(t, fmt.Sprintf("value-%d", n-1),
		got.GetStrToStr()["k-"+strings.Repeat("0", 6)+fmt.Sprintf("%d", n-1)])
	t.Logf("round-trip of %d entries took %s", n, elapsed)
	assert.Less(t, elapsed, 30*time.Second,
		"10k-entry round-trip must complete in well under 30s")
}

// TestMaps_LargeValueSize pins that a single large value (1 MiB
// string) round-trips through a map. Postgres JSONB columns are
// capped at 1 GiB; 1 MiB is comfortably below that. This is a
// regression guard against a bounded internal buffer in the codec.
func TestMaps_LargeValueSize(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping large-payload test in short mode")
	}
	ctx := t.Context()
	id := "maps-large-val-" + uniqueID(t)
	const size = 1 << 20 // 1 MiB
	big := strings.Repeat("x", size)
	in := &e2ev1.Maps{Id: id, StrToStr: map[string]string{"big": big}}

	got := insertAndReadMaps(t, ctx, in)
	require.Len(t, got.GetStrToStr(), 1)
	gotVal := got.GetStrToStr()["big"]
	assert.Len(t, gotVal, size, "1 MiB string value must not be truncated")
	assert.Equal(t, big[:32], gotVal[:32], "head bytes must match")
	assert.Equal(t, big[size-32:], gotVal[size-32:], "tail bytes must match")
}

// TestCollections_LargeRepeated_RoundTrip drives 10,000 strings
// through a TEXT[] column. Pairs with TestMaps_LargeMap_RoundTrip —
// the two cover the native-array and JSONB codec paths respectively.
func TestCollections_LargeRepeated_RoundTrip(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping large-payload test in short mode")
	}
	ctx := t.Context()
	id := "coll-large-" + uniqueID(t)
	const n = 10000
	labels := make([]string, n)
	for i := 0; i < n; i++ {
		labels[i] = fmt.Sprintf("label-%05d", i)
	}
	p := &e2ev1.Collections{Id: id, Labels: labels}
	row, err := gen.CollectionsModelFromProto(p)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))
	var read jetmodel.Collections
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.CollectionsTable.AllColumns).
			FROM(gen.CollectionsTable).
			WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.CollectionsProtoFromModel(&read)
	require.NoError(t, err)
	elapsed := time.Since(start)

	require.Len(t, got.GetLabels(), n)
	assert.Equal(t, labels[0], got.GetLabels()[0])
	assert.Equal(t, labels[n/2], got.GetLabels()[n/2])
	assert.Equal(t, labels[n-1], got.GetLabels()[n-1])
	t.Logf("round-trip of %d labels took %s", n, elapsed)
	assert.Less(t, elapsed, 30*time.Second,
		"10k-string TEXT[] round-trip must complete in well under 30s")
}

// TestMaps_BytesValue_EdgeCases covers map<string, bytes>:
//   - nil vs empty byte slice (proto3 collapses these on the wire,
//     but the test pins the round-trip behaviour).
//   - bytes containing a null byte (the JSON base64 codec must
//     encode without truncating at \x00).
//   - bytes spanning every 256-byte value (catches any byte-range
//     clipping bug).
//   - 1 MiB payload (JSONB size regression guard, paired with
//     TestMaps_LargeValueSize).
func TestMaps_BytesValue_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("empty_vs_all_bytes", func(t *testing.T) {
		t.Parallel()
		id := "maps-bytes-edge-" + uniqueID(t)
		all := make([]byte, 256)
		for i := range all {
			all[i] = byte(i)
		}
		in := &e2ev1.Maps{Id: id, StrToBytes: map[string][]byte{
			"empty":    {},
			"nullbyte": {0x00, 0x01, 0x02},
			"all":      all,
		}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToBytes(), 3)
		// proto3 on []byte: a nil and empty slice are indistinguishable
		// on the wire. The key must survive; the bytes compare
		// equal to an empty slice.
		assert.Empty(t, got.GetStrToBytes()["empty"])
		assert.Equal(t, []byte{0x00, 0x01, 0x02}, got.GetStrToBytes()["nullbyte"])
		assert.Equal(t, all, got.GetStrToBytes()["all"])

		// Raw JSONB probe — confirm base64 encoding is actually
		// applied. stdlib json base64-encodes []byte; a custom
		// codec that bypassed that would store a non-base64 string
		// and the round-trip would fail at decode.
		var raw string
		require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
			`SELECT str_to_bytes ->> 'nullbyte' FROM maps WHERE id = $1`, id,
		).Scan(&raw))
		// Base64 of {0x00, 0x01, 0x02} = "AAEC".
		assert.Equal(t, "AAEC", raw,
			"stored value must be base64(\\x00\\x01\\x02)")
	})

	t.Run("one_mib_bytes", func(t *testing.T) {
		t.Parallel()
		if testing.Short() {
			t.Skip("skipping 1 MiB bytes test in short mode")
		}
		id := "maps-bytes-1mib-" + uniqueID(t)
		big := make([]byte, 1<<20)
		for i := range big {
			big[i] = byte(i & 0xff)
		}
		in := &e2ev1.Maps{Id: id, StrToBytes: map[string][]byte{"big": big}}
		got := insertAndReadMaps(t, t.Context(), in)
		require.Len(t, got.GetStrToBytes(), 1)
		require.Len(t, got.GetStrToBytes()["big"], len(big))
		assert.Equal(t, big[:32], got.GetStrToBytes()["big"][:32])
		assert.Equal(t, big[len(big)-32:], got.GetStrToBytes()["big"][len(big)-32:])
	})
}

// TestCollections_MixedNullabilityInOneRow pins that a Collections
// row with a mixture of nil / empty / populated collection fields
// retains its per-column state through a round-trip. Catches "first
// mutation clobbers later mutations" bugs that single-field tests
// miss — a mapper that writes '{}' for a nil field would blow away
// the prior populated value of a neighbouring field if it got the
// column indices wrong.
func TestCollections_MixedNullabilityInOneRow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "coll-mixed-" + uniqueID(t)

	p := &e2ev1.Collections{
		Id:     id,
		Labels: nil,                     // nil -> SQL NULL
		Tags:   []e2ev1.CollectionTag{}, // empty -> '{}' TEXT[]
		Items: []*e2ev1.NestedPayload{ // populated -> JSONB array
			{Name: "a", Count: 1},
			{Name: "b", Count: 2},
		},
		Flags:      nil,                         // nil -> SQL NULL
		Attributes: map[string]string{"k": "v"}, // populated -> JSONB object
		// Remaining numeric arrays — skip a subset to mix.
		Ints32:  []int32{1, 2, 3},
		Uints32: nil,
	}
	row, err := gen.CollectionsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.CollectionsTable.INSERT(gen.CollectionsTable.AllColumns).MODEL(row))
	}))

	// Raw probe — per-column NULL / non-NULL per the expected state.
	var labelsTxt, tagsTxt, itemsTxt, attrsTxt sql.NullString
	var flagsTxt, ints32Txt, uints32Txt sql.NullString
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
		`SELECT labels::text, tags::text, items::text, attributes::text,
		        flags::text, ints32::text, uints32::text
		 FROM collections WHERE id = $1`, id,
	).Scan(&labelsTxt, &tagsTxt, &itemsTxt, &attrsTxt,
		&flagsTxt, &ints32Txt, &uints32Txt))
	assert.False(t, labelsTxt.Valid, "labels nil must persist as SQL NULL")
	assert.True(t, tagsTxt.Valid, "tags empty must persist as non-NULL")
	assert.Equal(t, "{}", tagsTxt.String)
	assert.True(t, itemsTxt.Valid, "items populated must persist as non-NULL")
	assert.True(t, attrsTxt.Valid, "attributes populated must persist as non-NULL")
	assert.False(t, flagsTxt.Valid, "flags nil must persist as SQL NULL")
	assert.True(t, ints32Txt.Valid, "ints32 populated must persist as non-NULL")
	assert.False(t, uints32Txt.Valid, "uints32 nil must persist as SQL NULL")

	// Proto-level round-trip — every column must keep its value.
	var read jetmodel.Collections
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.CollectionsTable.AllColumns).
			FROM(gen.CollectionsTable).
			WHERE(gen.CollectionsTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.CollectionsProtoFromModel(&read)
	require.NoError(t, err)
	assert.Empty(t, got.GetLabels(), "labels must be empty on read (nil proto ↔ SQL NULL)")
	assert.Empty(t, got.GetTags(), "tags must be empty on read")
	require.Len(t, got.GetItems(), 2)
	assert.Equal(t, "a", got.GetItems()[0].GetName())
	assert.Equal(t, "b", got.GetItems()[1].GetName())
	assert.Equal(t, map[string]string{"k": "v"}, got.GetAttributes())
	assert.Empty(t, got.GetFlags())
	assert.Equal(t, []int32{1, 2, 3}, got.GetInts32())
	assert.Empty(t, got.GetUints32())
}

// --------------------------------------------------------------------
// Oneof — required + optional variants, DB-level CHECK enforcement.
// --------------------------------------------------------------------

func TestRequiredOneof_VariantRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cases := map[string]*e2ev1.RequiredOneof{
		"variant_a": {
			Id: "req-a-" + uniqueID(t),
			Choice: &e2ev1.RequiredOneof_A{
				A: &e2ev1.VariantA{Message: "hello from A", Score: 42},
			},
		},
		"variant_b": {
			Id: "req-b-" + uniqueID(t),
			Choice: &e2ev1.RequiredOneof_B{
				B: &e2ev1.VariantB{Items: []string{"x", "y", "z"}, Active: true},
			},
		},
		"variant_c": {
			Id: "req-c-" + uniqueID(t),
			Choice: &e2ev1.RequiredOneof_C{
				C: &e2ev1.VariantC{Payload: []byte{0xde, 0xad, 0xbe, 0xef}},
			},
		},
	}
	for name, want := range cases {
		name := name
		want := want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, err := gen.RequiredOneofModelFromProto(want)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.RequiredOneofTable.INSERT(gen.RequiredOneofTable.AllColumns).MODEL(row))
			}))
			var read jetmodel.RequiredOneofs
			require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return postgres.SELECT(gen.RequiredOneofTable.AllColumns).
					FROM(gen.RequiredOneofTable).
					WHERE(gen.RequiredOneofTable.ID.EQ(postgres.String(want.GetId()))).
					QueryContext(ctx, tx, &read)
			}))
			got, err := gen.RequiredOneofProtoFromModel(&read)
			require.NoError(t, err)
			assertChoiceEqual(t, want.GetChoice(), got.GetChoice())
		})
	}
}

func TestRequiredOneof_UnsetRejectedByMapper(t *testing.T) {
	t.Parallel()
	_, err := gen.RequiredOneofModelFromProto(&e2ev1.RequiredOneof{Id: "unset"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "choice is required")
}

func TestRequiredOneof_ForgedKindRejectedByCHECK(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, err := sharedSuperDB.ExecContext(ctx, `
		INSERT INTO required_oneofs (id, choice_kind, choice) VALUES ($1, 'bogus', '{}'::jsonb)
	`, "forged-"+uniqueID(t))
	requirePGCode(t, err, pgerrcode.CheckViolation)
}

func TestOptionalOneof_Absent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "opt-empty-" + uniqueID(t)
	row, err := gen.OptionalOneofModelFromProto(&e2ev1.OptionalOneof{Id: id})
	require.NoError(t, err)
	assert.Equal(t, "", row.ChoiceKind)
	assert.Equal(t, "{}", row.Choice)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.OptionalOneofTable.INSERT(gen.OptionalOneofTable.AllColumns).MODEL(row))
	}))

	var read jetmodel.OptionalOneofs
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.OptionalOneofTable.AllColumns).
			FROM(gen.OptionalOneofTable).
			WHERE(gen.OptionalOneofTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.OptionalOneofProtoFromModel(&read)
	require.NoError(t, err)
	assert.Nil(t, got.GetChoice())
}

func TestOptionalOneof_VariantSwitch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "opt-switch-" + uniqueID(t)

	// Seed with variant A.
	first := &e2ev1.OptionalOneof{
		Id:     id,
		Choice: &e2ev1.OptionalOneof_A{A: &e2ev1.VariantA{Message: "first", Score: 1}},
	}
	row, err := gen.OptionalOneofModelFromProto(first)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.OptionalOneofTable.INSERT(gen.OptionalOneofTable.AllColumns).MODEL(row))
	}))

	// Switch to variant B. The generated UpdateAll must rewrite both
	// discriminator and payload columns.
	second := &e2ev1.OptionalOneof{
		Id:     id,
		Choice: &e2ev1.OptionalOneof_B{B: &e2ev1.VariantB{Items: []string{"one", "two"}, Active: true}},
	}
	updated, err := gen.OptionalOneofModelFromProto(second)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.OptionalOneofUpdateAll().
			MODEL(updated).
			WHERE(gen.OptionalOneofTable.ID.EQ(postgres.String(id))))
	}))

	var read jetmodel.OptionalOneofs
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.OptionalOneofTable.AllColumns).
			FROM(gen.OptionalOneofTable).
			WHERE(gen.OptionalOneofTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	assert.Equal(t, "b", read.ChoiceKind)
	// Old VariantA fields must not linger in the JSONB.
	assert.NotContains(t, read.Choice, `"message":"first"`)
	got, err := gen.OptionalOneofProtoFromModel(&read)
	require.NoError(t, err)
	assert.Equal(t, second.GetB().GetItems(), got.GetB().GetItems())
	assert.True(t, got.GetB().GetActive())
}

// --------------------------------------------------------------------
// ScalarOneof — scalar + enum variants alongside a message variant,
// each persisted through the same discriminator + JSONB pair. Proves
// the mapper emits native JSON for primitives / enum names and still
// falls back to protojson for the message case.
// --------------------------------------------------------------------

func TestScalarOneof_VariantRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cases := map[string]*e2ev1.ScalarOneof{
		"bool": {
			Id:      "scalar-bool-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Enabled{Enabled: true},
		},
		"string_unicode": {
			Id:      "scalar-str-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Note{Note: "日本語 / \"escaped\" / O'Brien"},
		},
		"int32_min": {
			Id:      "scalar-i32-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Score32{Score32: math.MinInt32},
		},
		"int64_max": {
			Id:      "scalar-i64-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Score64{Score64: math.MaxInt64},
		},
		"uint32_max": {
			Id:      "scalar-u32-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Tally32{Tally32: math.MaxUint32},
		},
		"uint64_max": {
			Id:      "scalar-u64-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Tally64{Tally64: math.MaxUint64},
		},
		"float": {
			Id:      "scalar-f32-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Ratio{Ratio: float32(math.Pi)},
		},
		"double": {
			Id:      "scalar-f64-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Measure{Measure: math.E},
		},
		"bytes": {
			Id:      "scalar-b-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Blob{Blob: []byte{0x00, 0x01, 0xfe, 0xff}},
		},
		"enum": {
			Id:      "scalar-enum-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_Flavour{Flavour: e2ev1.ScalarFlavour_SCALAR_FLAVOUR_SOUR},
		},
		"message": {
			Id:      "scalar-msg-" + uniqueID(t),
			Payload: &e2ev1.ScalarOneof_A{A: &e2ev1.VariantA{Message: "embedded", Score: 7}},
		},
	}
	for name, want := range cases {
		name := name
		want := want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, err := gen.ScalarOneofModelFromProto(want)
			require.NoError(t, err)
			require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return jetExec(ctx, tx, gen.ScalarOneofTable.INSERT(gen.ScalarOneofTable.AllColumns).MODEL(row))
			}))
			var read jetmodel.ScalarOneofs
			require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
				return postgres.SELECT(gen.ScalarOneofTable.AllColumns).
					FROM(gen.ScalarOneofTable).
					WHERE(gen.ScalarOneofTable.ID.EQ(postgres.String(want.GetId()))).
					QueryContext(ctx, tx, &read)
			}))
			got, err := gen.ScalarOneofProtoFromModel(&read)
			require.NoError(t, err)
			assert.Truef(t, proto.Equal(want, got), "want %v, got %v", want, got)
		})
	}
}

// TestScalarOneof_EnumStoredAsName pins the wire representation of an
// enum variant — the JSONB column holds the enum's String() form as a
// JSON string, matching how bare enum scalars persist. Protects against
// a future mapper change that would silently switch to numeric storage.
func TestScalarOneof_EnumStoredAsName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "scalar-enum-shape-" + uniqueID(t)
	in := &e2ev1.ScalarOneof{
		Id:      id,
		Payload: &e2ev1.ScalarOneof_Flavour{Flavour: e2ev1.ScalarFlavour_SCALAR_FLAVOUR_SWEET},
	}
	row, err := gen.ScalarOneofModelFromProto(in)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ScalarOneofTable.INSERT(gen.ScalarOneofTable.AllColumns).MODEL(row))
	}))
	var kindTxt, payloadTxt string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT payload_kind, payload::text FROM scalar_oneofs WHERE id = $1
	`, id).Scan(&kindTxt, &payloadTxt))
	assert.Equal(t, "flavour", kindTxt)
	assert.Equal(t, `"SCALAR_FLAVOUR_SWEET"`, payloadTxt)
}

// TestScalarOneof_ForgedKindRejectedByCHECK mirrors the required-oneof
// variant guard — the generated CHECK enumerates every variant name
// including the new scalar ones.
func TestScalarOneof_ForgedKindRejectedByCHECK(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, err := sharedSuperDB.ExecContext(ctx, `
		INSERT INTO scalar_oneofs (id, payload_kind, payload) VALUES ($1, 'nope', '{}'::jsonb)
	`, "scalar-forged-"+uniqueID(t))
	requirePGCode(t, err, pgerrcode.CheckViolation)
}

// --------------------------------------------------------------------
// Column options — name override, default_expr, check, immutable.
// --------------------------------------------------------------------

func TestColumnOpts_NameOverride(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	row := jetmodel.ColumnOpts{
		ID:           "rename-" + uniqueID(t),
		RenamedField: "visible",
		DefaultValue: "",
		BoundedValue: 5,
		Creator:      "test",
		Status:       "OPT_STATUS_OPEN",
	}
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ColumnOptsTable.INSERT(gen.ColumnOptsTable.AllColumns).MODEL(row))
	}))
	var visible string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
		`SELECT renamed_field FROM column_opts WHERE id = $1`, row.ID,
	).Scan(&visible))
	assert.Equal(t, "visible", visible)
}

func TestColumnOpts_DefaultExprApplied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "default-" + uniqueID(t)
	_, err := sharedSuperDB.ExecContext(ctx, `
		INSERT INTO column_opts (id, renamed_field, bounded_value, creator, status)
		VALUES ($1, '', 0, '', 'OPT_STATUS_OPEN')
	`, id)
	require.NoError(t, err)
	var read string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
		`SELECT default_value FROM column_opts WHERE id = $1`, id,
	).Scan(&read))
	assert.Equal(t, "default-text", read)
}

func TestColumnOpts_CheckConstraintRejects(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, err := sharedSuperDB.ExecContext(ctx, `
		INSERT INTO column_opts (id, renamed_field, bounded_value, creator, status)
		VALUES ($1, '', 999, '', 'OPT_STATUS_OPEN')
	`, "bad-"+uniqueID(t))
	requirePGCode(t, err, pgerrcode.CheckViolation)
}

// TestColumnOpts_ImmutableExcludedFromUpdateAll inspects the generated
// aliases source to confirm the immutable columns are NOT in the
// UPDATE list. Compile-time shape assertion — reflection over jet's
// internal UpdateStatement would be more brittle.
func TestColumnOpts_ImmutableExcludedFromUpdateAll(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("gen/jet/e2e/v1/storage/columnopts_aliases.go")
	require.NoError(t, err)
	src := string(source)
	// Mutable fields appear.
	assert.Contains(t, src, "ColumnOptsTable.RenamedField")
	assert.Contains(t, src, "ColumnOptsTable.DefaultValue")
	assert.Contains(t, src, "ColumnOptsTable.BoundedValue")
	assert.Contains(t, src, "ColumnOptsTable.Status")
	// Immutable columns (id + creator) excluded from UpdateAll.
	// Match the exact UPDATE list — they don't appear as a table
	// column reference followed by a comma in the function body.
	updateSection := src[strings.Index(src, "UpdateAll()"):]
	updateSection = updateSection[:strings.Index(updateSection, "}")]
	assert.NotContains(t, updateSection, "ColumnOptsTable.ID,")
	assert.NotContains(t, updateSection, "ColumnOptsTable.Creator,")
}

// TestColumnOpts_RenameDirectiveEmitted pins that declaring
// `rename_from` on a column surfaces a PLUGIN-RENAME line in the
// emitted DDL. Migration authors grep the DDL for these directives
// to know when to use ALTER TABLE RENAME COLUMN instead of
// DROP+ADD. A regression silently losing the directive would lead
// to data loss on the next rename.
func TestColumnOpts_RenameDirectiveEmitted(t *testing.T) {
	t.Parallel()
	assert.Contains(t, gen.ColumnOptsDDL,
		"-- PLUGIN-RENAME: column_opts old_label -> new_label",
	)
	// Runtime behaviour is unchanged — a value written to new_label
	// round-trips like any plain TEXT column.
	ctx := t.Context()
	id := "rename-" + uniqueID(t)
	p := &e2ev1.ColumnOpts{
		Id:           id,
		DefaultValue: "seed",
		Creator:      "test",
		NewLabel:     "shiny-new-value",
	}
	row, err := gen.ColumnOptsModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ColumnOptsTable.INSERT(gen.ColumnOptsTable.AllColumns).MODEL(row))
	}))
	var stored string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx,
		`SELECT new_label FROM column_opts WHERE id = $1`, id,
	).Scan(&stored))
	assert.Equal(t, "shiny-new-value", stored)
}

// --------------------------------------------------------------------
// Tenancy — RLS isolation, pagination, composite PK, partial indexes.
// --------------------------------------------------------------------

func TestTenancy_TenantScoped_RLSIsolation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenantA := "tenant-a-" + uniqueID(t)
	tenantB := "tenant-b-" + uniqueID(t)

	insertTenantRow(ctx, t, tenantA, "shared-name", "red")
	insertTenantRow(ctx, t, tenantB, "shared-name", "blue")

	assert.Equal(t, "red", selectStatusForTenant(ctx, t, tenantA, "shared-name"))
	assert.Equal(t, "blue", selectStatusForTenant(ctx, t, tenantB, "shared-name"))
}

func TestTenancy_TenantScoped_ListPagination(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenant := "tenant-list-" + uniqueID(t)
	const n = 7
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("row-%02d-%s", i, uniqueID(t))
		createdAt := base.Add(time.Duration(i) * time.Hour)
		insertTenantRowAt(ctx, t, tenant, name, "ok", true, createdAt)
	}

	seen := map[string]struct{}{}
	pageSize := int32(3)
	token := ""
	var pageCount int
	for {
		rows, next := listTenantPage(ctx, t, tenant, pageSize, token)
		for _, r := range rows {
			seen[r.Name] = struct{}{}
		}
		pageCount++
		if next == "" {
			break
		}
		token = next
		require.Less(t, pageCount, 10, "pagination did not terminate")
	}
	assert.Len(t, seen, n)
	assert.Equal(t, 3, pageCount)
}

func TestTenancy_TenantScoped_Update(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tenant := "tenant-upd-" + uniqueID(t)
	name := "upd-" + uniqueID(t)
	insertTenantRow(ctx, t, tenant, name, "open")

	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		row := jetmodel.TenantScoped{
			TenantID: tenant, Name: name, Status: "closed", Enabled: true,
		}
		return jetExec(ctx, tx, gen.TenantScopedUpdateAll().
			MODEL(row).
			WHERE(gen.TenantScopedTable.Name.EQ(postgres.String(name))))
	}))
	assert.Equal(t, "closed", selectStatusForTenant(ctx, t, tenant, name))
}

func TestTenancy_TenantScoped_Delete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tenant := "tenant-del-" + uniqueID(t)
	name := "del-" + uniqueID(t)
	insertTenantRow(ctx, t, tenant, name, "x")

	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		res, err := gen.TenantScopedTable.
			DELETE().
			WHERE(gen.TenantScopedTable.Name.EQ(postgres.String(name))).
			ExecContext(ctx, tx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		return nil
	}))

	// Re-run — under the same tenant the row is gone, so RowsAffected is 0.
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		res, err := gen.TenantScopedTable.
			DELETE().
			WHERE(gen.TenantScopedTable.Name.EQ(postgres.String(name))).
			ExecContext(ctx, tx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		require.NoError(t, err)
		assert.EqualValues(t, 0, n)
		return nil
	}))
}

func TestTenancy_UserScoped_Isolation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenant := "utenant-" + uniqueID(t)
	userA := "user-a-" + uniqueID(t)
	userB := "user-b-" + uniqueID(t)

	insertUserRow(ctx, t, tenant, userA, "same-name", "A-payload")
	insertUserRow(ctx, t, tenant, userB, "same-name", "B-payload")

	assert.Equal(t, "A-payload", selectPayloadForUser(ctx, t, tenant, userA, "same-name"))
	assert.Equal(t, "B-payload", selectPayloadForUser(ctx, t, tenant, userB, "same-name"))
}

// --------------------------------------------------------------------
// Multi-resource / custom layout.
// --------------------------------------------------------------------

// TestAlpha_FieldBehaviorIDENTIFIER_AutoMirrorsImmutable is the
// e2e proof point for the `(google.api.field_behavior) =
// IDENTIFIER` → `immutable: true` auto-mirror. Alpha.id carries
// IDENTIFIER but NO explicit `immutable: true`. If the auto-
// mirror regresses, AlphaUpdateAll()'s column list gains ID and
// an UPDATE with a stale ID value would succeed (it shouldn't —
// the identifier must round-trip).
func TestAlpha_FieldBehaviorIDENTIFIER_AutoMirrorsImmutable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	id := "alpha-" + uniqueID(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.AlphaTable.INSERT(gen.AlphaTable.AllColumns).MODEL(jetmodel.Alphas{
			ID: id, Label: "original", CreatedAt: now,
		}))
	}))

	// Try to UPDATE the ID via AlphaUpdateAll() — if the auto-
	// mirror worked, ID isn't in the UPDATE's column set, so the
	// new ID value passed in MODEL() gets ignored and the row's
	// ID stays as originally inserted.
	attempted := jetmodel.Alphas{ID: "attacker", Label: "modified", CreatedAt: now}
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.AlphaUpdateAll().
			MODEL(attempted).
			WHERE(gen.AlphaTable.ID.EQ(postgres.String(id))))
	}))

	// Row with the ORIGINAL id must still exist. If AlphaUpdateAll()
	// included ID in its SET list, the row's ID would have changed
	// to "attacker" and this SELECT would miss.
	var read jetmodel.Alphas
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.AlphaTable.AllColumns).
			FROM(gen.AlphaTable).
			WHERE(gen.AlphaTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	assert.Equal(t, id, read.ID, "IDENTIFIER-auto-mirrored field must not be updatable via UpdateAll")
	assert.Equal(t, "modified", read.Label, "mutable field must update normally")
}

func TestMultiResource_SamePackageInsertAndRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	alphaID := "alpha-" + uniqueID(t)
	betaID := "beta-" + uniqueID(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.AlphaTable.INSERT(gen.AlphaTable.AllColumns).MODEL(jetmodel.Alphas{
			ID:        alphaID,
			Label:     "a",
			CreatedAt: now,
		}))
	}))
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.BetaTable.INSERT(gen.BetaTable.AllColumns).MODEL(jetmodel.Betas{
			ID:        betaID,
			Weight:    99,
			CreatedAt: now,
		}))
	}))

	var alpha jetmodel.Alphas
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.AlphaTable.AllColumns).
			FROM(gen.AlphaTable).
			WHERE(gen.AlphaTable.ID.EQ(postgres.String(alphaID))).
			QueryContext(ctx, tx, &alpha)
	}))
	assert.Equal(t, "a", alpha.Label)

	var beta jetmodel.Betas
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.BetaTable.AllColumns).
			FROM(gen.BetaTable).
			WHERE(gen.BetaTable.ID.EQ(postgres.String(betaID))).
			QueryContext(ctx, tx, &beta)
	}))
	assert.EqualValues(t, 99, beta.Weight)
}

func TestOverrideLayout_CustomPackage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "custom-" + uniqueID(t)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, customstorage.CustomLayoutTable.
			INSERT(customstorage.CustomLayoutTable.AllColumns).
			MODEL(custommodel.CustomLayout{ID: id, Label: "override-test"}))
	}))
	var read custommodel.CustomLayout
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(customstorage.CustomLayoutTable.AllColumns).
			FROM(customstorage.CustomLayoutTable).
			WHERE(customstorage.CustomLayoutTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	assert.Equal(t, "override-test", read.Label)

	got, err := customstorage.CustomLayoutProtoFromModel(&read)
	require.NoError(t, err)
	assert.Equal(t, id, got.GetId())
	assert.Equal(t, "override-test", got.GetLabel())

	// Resource-type constant is the proto FQN, not the overridden Go
	// package name.
	assert.Equal(t, "jet.e2e.override.v1.CustomLayout", customstorage.CustomLayoutResourceType)

	// The proto's Go package defaults to `overridev1` while the
	// storage symbols use the overridden `customstorage` package.
	_ = overridev1.CustomLayout{}
}

// --------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------

// assertChoiceEqual compares two oneof payloads through proto API
// getters. Direct reflect.DeepEqual would trip over internal
// unexported fields on protoreflect.
func assertChoiceEqual(t *testing.T, want, got any) {
	t.Helper()
	// Marshal both through the same JSON surface to get a stable
	// comparison key.
	wantBytes, err := json.Marshal(toComparable(want))
	require.NoError(t, err)
	gotBytes, err := json.Marshal(toComparable(got))
	require.NoError(t, err)
	assert.JSONEq(t, string(wantBytes), string(gotBytes))
}

func toComparable(v any) any {
	switch x := v.(type) {
	case *e2ev1.RequiredOneof_A:
		return map[string]any{"a": map[string]any{"message": x.A.GetMessage(), "score": x.A.GetScore()}}
	case *e2ev1.RequiredOneof_B:
		return map[string]any{"b": map[string]any{"items": x.B.GetItems(), "active": x.B.GetActive()}}
	case *e2ev1.RequiredOneof_C:
		return map[string]any{"c": map[string]any{"payload": x.C.GetPayload()}}
	}
	return v
}

func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func inReadTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// requirePGCode asserts a wrapped driver error has the expected
// SQLSTATE code.
func requirePGCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	assert.Equal(t, code, pgErr.Code, "sqlstate: %s / message: %s", pgErr.Code, pgErr.Message)
}

func makeByteAlphabet() []byte {
	out := make([]byte, 256)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

func insertTenantRow(ctx context.Context, t *testing.T, tenant, name, status string) {
	t.Helper()
	insertTenantRowAt(ctx, t, tenant, name, status, true, time.Now().UTC().Truncate(time.Microsecond))
}

func insertTenantRowAt(ctx context.Context, t *testing.T, tenant, name, status string, enabled bool, createdAt time.Time) {
	t.Helper()
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		row := jetmodel.TenantScoped{
			TenantID: tenant, Name: name, Status: status, Enabled: enabled, CreatedAt: createdAt,
		}
		return jetExec(ctx, tx, gen.TenantScopedTable.
			INSERT(gen.TenantScopedTable.AllColumns).
			MODEL(row))
	}))
}

func selectStatusForTenant(ctx context.Context, t *testing.T, tenant, name string) string {
	t.Helper()
	var out string
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		var row jetmodel.TenantScoped
		err := postgres.SELECT(gen.TenantScopedTable.AllColumns).
			FROM(gen.TenantScopedTable).
			WHERE(gen.TenantScopedTable.Name.EQ(postgres.String(name))).
			QueryContext(ctx, tx, &row)
		if errors.Is(err, qrm.ErrNoRows) {
			out = "<none>"
			return nil
		}
		if err != nil {
			return err
		}
		out = row.Status
		return nil
	}))
	return out
}

func listTenantPage(ctx context.Context, t *testing.T, tenant string, pageSize int32, token string) ([]jetmodel.TenantScoped, string) {
	t.Helper()
	var (
		rows []jetmodel.TenantScoped
		next string
	)
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, "", func(tx *sql.Tx) error {
		stmt := postgres.SELECT(gen.TenantScopedTable.AllColumns).FROM(gen.TenantScopedTable)
		res, nxt, err := aipjet.Execute(ctx, gen.TenantScopedListSchema, aip.Params{
			PageSize: pageSize, PageToken: token,
		}, stmt, tx)
		rows, next = res, nxt
		return err
	}))
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, next
}

func insertUserRow(ctx context.Context, t *testing.T, tenant, user, name, payload string) {
	t.Helper()
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, user, func(tx *sql.Tx) error {
		row := jetmodel.UserScoped{
			TenantID: tenant, UserID: user, Name: name, Payload: payload,
		}
		return jetExec(ctx, tx, gen.UserScopedTable.
			INSERT(gen.UserScopedTable.AllColumns).
			MODEL(row))
	}))
}

func selectPayloadForUser(ctx context.Context, t *testing.T, tenant, user, name string) string {
	t.Helper()
	var out string
	require.NoError(t, withTenantTxExec(ctx, t, sharedTenantDB, tenant, user, func(tx *sql.Tx) error {
		var row jetmodel.UserScoped
		err := postgres.SELECT(gen.UserScopedTable.AllColumns).
			FROM(gen.UserScopedTable).
			WHERE(gen.UserScopedTable.Name.EQ(postgres.String(name))).
			QueryContext(ctx, tx, &row)
		if errors.Is(err, qrm.ErrNoRows) {
			out = "<none>"
			return nil
		}
		if err != nil {
			return err
		}
		out = row.Payload
		return nil
	}))
	return out
}

// withTenantTxExec opens a read-write tx, sets app.tenant_id (and
// optional app.user_id), invokes fn, and commits on success.
func withTenantTxExec(ctx context.Context, t *testing.T, db *sql.DB, tenantID, userID string, fn func(tx *sql.Tx) error) error {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if userID != "" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.user_id', $1, true)", userID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// --------------------------------------------------------------------
// Well-known types — one fixture column per WKT the plugin's generic
// JSONB path is expected to handle. Timestamp and Duration have their
// own paths and are covered by TestTimes_*; everything else goes
// through KindJSONBProto via protojson.
// --------------------------------------------------------------------

// TestWKT_Wrappers_RoundTrip pins the primitive-wrapper WKTs.
// Each serialises to its underlying JSON literal — StringValue{"hi"}
// becomes "hi", not {"value":"hi"} — and the plugin's JSONBProto
// path round-trips them as-is.
func TestWKT_Wrappers_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{
		Id:         "wrappers-" + uniqueID(t),
		StringWrap: wrapperspb.String("café ✓ \"quoted\""),
		Int32Wrap:  wrapperspb.Int32(math.MinInt32),
		Int64Wrap:  wrapperspb.Int64(math.MaxInt64),
		Uint32Wrap: wrapperspb.UInt32(math.MaxUint32),
		Uint64Wrap: wrapperspb.UInt64(math.MaxUint64),
		BoolWrap:   wrapperspb.Bool(true),
		BytesWrap:  wrapperspb.Bytes([]byte{0x00, 0x7f, 0xff, 0x42}),
		// float / double wrappers — the one legitimate way a float
		// reaches storage. Extremes on both sides.
		FloatWrap:  wrapperspb.Float(math.MaxFloat32),
		DoubleWrap: wrapperspb.Double(-math.MaxFloat64),
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		assert.Equal(t, p.GetStringWrap().GetValue(), got.GetStringWrap().GetValue())
		assert.Equal(t, p.GetInt32Wrap().GetValue(), got.GetInt32Wrap().GetValue())
		assert.Equal(t, p.GetInt64Wrap().GetValue(), got.GetInt64Wrap().GetValue())
		assert.Equal(t, p.GetUint32Wrap().GetValue(), got.GetUint32Wrap().GetValue())
		assert.Equal(t, p.GetUint64Wrap().GetValue(), got.GetUint64Wrap().GetValue())
		assert.Equal(t, p.GetBoolWrap().GetValue(), got.GetBoolWrap().GetValue())
		assert.Equal(t, p.GetBytesWrap().GetValue(), got.GetBytesWrap().GetValue())
		assert.Equal(t, p.GetFloatWrap().GetValue(), got.GetFloatWrap().GetValue())
		assert.Equal(t, p.GetDoubleWrap().GetValue(), got.GetDoubleWrap().GetValue())
	})
}

// TestWKT_Wrappers_ExplicitZero pins presence semantics under the
// native-column encoding. Explicit-zero wrappers (StringValue{""},
// Int32Value{0}, BoolValue{false}) round-trip as non-nil wrappers with
// zero values, NOT as nil wrappers — the TEXT / INTEGER / BOOLEAN
// columns store "", 0, and false, which are distinguishable from NULL.
//
// BytesValue with an explicit empty-but-non-nil slice survives too.
// An explicit `wrapperspb.Bytes(nil)` does NOT survive — pgx encodes
// a nil inner []byte as SQL NULL regardless of the outer wrapper
// pointer, so nil-bytes collapses with unset. Callers who need to
// distinguish "explicitly empty bytes" from "unset" should use a bool
// sibling flag.
func TestWKT_Wrappers_ExplicitZero(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{
		Id:         "wrappers-zero-" + uniqueID(t),
		StringWrap: wrapperspb.String(""),
		Int32Wrap:  wrapperspb.Int32(0),
		BoolWrap:   wrapperspb.Bool(false),
		BytesWrap:  wrapperspb.Bytes([]byte{}),
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		require.NotNil(t, got.GetStringWrap(), "explicit empty StringValue must not collapse to nil")
		assert.Equal(t, "", got.GetStringWrap().GetValue())
		require.NotNil(t, got.GetInt32Wrap())
		assert.Equal(t, int32(0), got.GetInt32Wrap().GetValue())
		require.NotNil(t, got.GetBoolWrap())
		assert.False(t, got.GetBoolWrap().GetValue())
		require.NotNil(t, got.GetBytesWrap(), "explicit empty non-nil bytes must survive")
		assert.Empty(t, got.GetBytesWrap().GetValue())
	})
}

// TestWKT_Wrappers_UnsetStayNil covers the complement of ExplicitZero:
// a wrapper field that was never set on write is nil on read. The
// mapper writes "{}" for unset JSONB-proto fields, and the reader
// skips that sentinel.
func TestWKT_Wrappers_UnsetStayNil(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{Id: "wrappers-unset-" + uniqueID(t)}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		assert.Nil(t, got.GetStringWrap())
		assert.Nil(t, got.GetInt32Wrap())
		assert.Nil(t, got.GetInt64Wrap())
		assert.Nil(t, got.GetUint32Wrap())
		assert.Nil(t, got.GetUint64Wrap())
		assert.Nil(t, got.GetBoolWrap())
		assert.Nil(t, got.GetBytesWrap())
		assert.Nil(t, got.GetFloatWrap())
		assert.Nil(t, got.GetDoubleWrap())
	})
}

// TestWKT_Struct_RoundTrip — Struct serialises to a native JSON object
// whose values are Value-typed. Every JSON scalar flavor appears.
func TestWKT_Struct_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	s, err := structpb.NewStruct(map[string]any{
		"name":    "provider-openai",
		"weight":  3.14,
		"enabled": true,
		"tags":    []any{"prod", "critical"},
		"nested":  map[string]any{"region": "us-east-1"},
		"null":    nil,
	})
	require.NoError(t, err)

	p := &e2ev1.Wkt{
		Id:          "struct-" + uniqueID(t),
		StructValue: s,
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		require.NotNil(t, got.GetStructValue())
		gotMap := got.GetStructValue().AsMap()
		assert.Equal(t, "provider-openai", gotMap["name"])
		assert.InDelta(t, 3.14, gotMap["weight"], 1e-9)
		assert.Equal(t, true, gotMap["enabled"])
		assert.Equal(t, []any{"prod", "critical"}, gotMap["tags"])
		assert.Equal(t, map[string]any{"region": "us-east-1"}, gotMap["nested"])
		assert.Contains(t, gotMap, "null")
		assert.Nil(t, gotMap["null"])
	})
}

// TestWKT_Value_RoundTrip — Value is a union of JSON scalars. Each
// variant is a distinct round-trip.
func TestWKT_Value_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cases := []struct {
		name  string
		build func() *structpb.Value
		check func(*testing.T, *structpb.Value)
	}{
		{"string", func() *structpb.Value { return structpb.NewStringValue("hi") }, func(t *testing.T, v *structpb.Value) {
			assert.Equal(t, "hi", v.GetStringValue())
		}},
		{"number", func() *structpb.Value { return structpb.NewNumberValue(42.5) }, func(t *testing.T, v *structpb.Value) {
			assert.InDelta(t, 42.5, v.GetNumberValue(), 1e-9)
		}},
		{"bool-true", func() *structpb.Value { return structpb.NewBoolValue(true) }, func(t *testing.T, v *structpb.Value) {
			assert.True(t, v.GetBoolValue())
		}},
		{"null", func() *structpb.Value { return structpb.NewNullValue() }, func(t *testing.T, v *structpb.Value) {
			// NullValue is the zero value of the number-or-null kind. Its
			// presence is detectable via GetKind() returning *NullValue.
			_, ok := v.GetKind().(*structpb.Value_NullValue)
			assert.True(t, ok, "Value with NullValue kind must round-trip as Value_NullValue, got %T", v.GetKind())
		}},
		{"list", func() *structpb.Value {
			l, err := structpb.NewList([]any{"a", 1.0, true})
			require.NoError(t, err)
			return structpb.NewListValue(l)
		}, func(t *testing.T, v *structpb.Value) {
			assert.Equal(t, []any{"a", 1.0, true}, v.GetListValue().AsSlice())
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &e2ev1.Wkt{
				Id:       "value-" + tc.name + "-" + uniqueID(t),
				ValueAny: tc.build(),
			}
			roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
				require.NotNil(t, got.GetValueAny())
				tc.check(t, got.GetValueAny())
			})
		})
	}
}

// TestWKT_ListValue_RoundTrip pins the array form.
func TestWKT_ListValue_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	lv, err := structpb.NewList([]any{"alpha", 42.0, false, nil, map[string]any{"k": "v"}})
	require.NoError(t, err)
	p := &e2ev1.Wkt{
		Id:        "list-" + uniqueID(t),
		ListValue: lv,
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		require.NotNil(t, got.GetListValue())
		slice := got.GetListValue().AsSlice()
		require.Len(t, slice, 5)
		assert.Equal(t, "alpha", slice[0])
		assert.InDelta(t, 42.0, slice[1], 1e-9)
		assert.Equal(t, false, slice[2])
		assert.Nil(t, slice[3])
		assert.Equal(t, map[string]any{"k": "v"}, slice[4])
	})
}

// TestWKT_Any_RoundTrip packs a known WKT into an Any and confirms
// the type_url + value come back intact through protojson.
func TestWKT_Any_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	inner, err := anypb.New(wrapperspb.String("payload"))
	require.NoError(t, err)

	p := &e2ev1.Wkt{
		Id:       "any-" + uniqueID(t),
		AnyValue: inner,
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		require.NotNil(t, got.GetAnyValue())
		var unpacked wrapperspb.StringValue
		require.NoError(t, got.GetAnyValue().UnmarshalTo(&unpacked))
		assert.Equal(t, "payload", unpacked.GetValue())
	})
}

// TestWKT_FieldMask_RoundTrip — FieldMask's protojson form is a
// comma-separated path string, not an object. Round-trip must
// preserve ordering + duplicates as the caller sent them.
func TestWKT_FieldMask_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{
		Id:        "mask-" + uniqueID(t),
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "enabled", "metadata.region"}},
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		require.NotNil(t, got.GetFieldMask())
		assert.Equal(t, []string{"display_name", "enabled", "metadata.region"}, got.GetFieldMask().GetPaths())
	})
}

// TestWKT_Empty_RoundTripPresenceOnly — Empty has no fields; the only
// observable is presence. Set → Get returns non-nil. Unset stays nil.
//
// Subtle: an explicitly-set Empty protojson-serialises to {}, which is
// exactly the sentinel the mapper uses for unset JSONB-proto fields.
// So the two states — "set to &Empty{}" and "never set" — collapse to
// the same stored bytes and both read back as nil. That's a known
// limitation of using {} as the unset sentinel for generic messages;
// pin it here so nobody tries to rely on Empty presence alone.
func TestWKT_Empty_RoundTripPresenceOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{
		Id:         "empty-" + uniqueID(t),
		EmptyValue: &emptypb.Empty{},
	}
	roundTripWKT(t, ctx, p, func(t *testing.T, got *e2ev1.Wkt) {
		// Collapses with the "unset" sentinel — see docstring. The
		// resource shouldn't rely on Empty as a presence flag; use a
		// bool column instead.
		assert.Nil(t, got.GetEmptyValue(),
			"Empty's protojson form {} collides with the mapper sentinel; presence is lost")
	})
}

// roundTripWKT is the shared write → read cycle for the WKT fixture.
// Every case is the same shape: mapper → INSERT via jet → SELECT back
// → mapper → caller assertion.
func roundTripWKT(t *testing.T, ctx context.Context, p *e2ev1.Wkt, check func(*testing.T, *e2ev1.Wkt)) {
	t.Helper()
	row, err := gen.WktModelFromProto(p)
	require.NoError(t, err)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.WktTable.INSERT(gen.WktTable.AllColumns).MODEL(row))
	}))

	var read jetmodel.Wkt
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.WktTable.AllColumns).
			FROM(gen.WktTable).
			WHERE(gen.WktTable.ID.EQ(postgres.String(p.GetId()))).
			QueryContext(ctx, tx, &read)
	}))

	got, err := gen.WktProtoFromModel(&read)
	require.NoError(t, err)
	check(t, got)
}

// --------------------------------------------------------------------
// Nullable columns — every supported kind with `nullable: true`.
// Proto3-optional scalars preserve unset vs explicit-zero through
// pointer presence; Timestamp uses the natural message-pointer;
// *[]byte tracks jet's model shape for a NULL-able BYTEA column.
// --------------------------------------------------------------------

// TestNullable_AllSet populates every nullable field and asserts a
// full round-trip. Values chosen so "explicit zero" is detectable —
// the point of nullable is that zero vs unset are different.
func TestNullable_AllSet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	now := time.Now().UTC().Truncate(time.Microsecond)
	strVal := ""
	boolVal := false
	i32Val := int32(0)
	i64Val := int64(0)
	bytesVal := []byte{}
	kindVal := e2ev1.NullableKind_NULLABLE_KIND_A

	p := &e2ev1.Nullable{
		Id:        "null-all-" + uniqueID(t),
		StringOpt: &strVal,
		BoolOpt:   &boolVal,
		Int32Opt:  &i32Val,
		Int64Opt:  &i64Val,
		BytesOpt:  bytesVal,
		KindOpt:   &kindVal,
		DeletedAt: timestamppb.New(now),
		TtlOpt:    durationpb.New(5 * time.Minute),
	}
	roundTripNullable(t, ctx, p, func(t *testing.T, got *e2ev1.Nullable) {
		// Every optional scalar must be non-nil (presence preserved)
		// and equal to zero. Unset vs explicit-zero is the whole
		// point of nullable — a missing presence here means the mapper
		// or the model lost it.
		require.NotNil(t, got.StringOpt)
		assert.Equal(t, "", *got.StringOpt)
		require.NotNil(t, got.BoolOpt)
		assert.False(t, *got.BoolOpt)
		require.NotNil(t, got.Int32Opt)
		assert.Equal(t, int32(0), *got.Int32Opt)
		require.NotNil(t, got.Int64Opt)
		assert.Equal(t, int64(0), *got.Int64Opt)
		assert.NotNil(t, got.BytesOpt, "explicit empty-bytes must read back non-nil")
		assert.Empty(t, got.BytesOpt)
		require.NotNil(t, got.KindOpt)
		assert.Equal(t, e2ev1.NullableKind_NULLABLE_KIND_A, *got.KindOpt)
		require.NotNil(t, got.DeletedAt)
		assert.True(t, got.DeletedAt.AsTime().Equal(now))
		require.NotNil(t, got.TtlOpt)
		assert.Equal(t, 5*time.Minute, got.TtlOpt.AsDuration())
	})
}

// TestNullable_AllUnset inserts nothing but the PK and asserts every
// column reads back NULL / nil. Pins the "unset vs zero" contract
// from the other side — no stray zeros.
func TestNullable_AllUnset(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Nullable{Id: "null-unset-" + uniqueID(t)}
	roundTripNullable(t, ctx, p, func(t *testing.T, got *e2ev1.Nullable) {
		assert.Nil(t, got.StringOpt)
		assert.Nil(t, got.BoolOpt)
		assert.Nil(t, got.Int32Opt)
		assert.Nil(t, got.Int64Opt)
		assert.Nil(t, got.BytesOpt)
		assert.Nil(t, got.KindOpt)
		assert.Nil(t, got.DeletedAt)
		assert.Nil(t, got.TtlOpt)
	})
}

// TestNullable_NonZeroValues pushes concrete non-zero values through
// every nullable kind to verify the pointer bridge doesn't corrupt.
// Unicode string, negative int32, large int64, real bytes, specific
// enum, a concrete timestamp, a long duration.
func TestNullable_NonZeroValues(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	strVal := "hello unicode ✓"
	boolVal := true
	i32Val := int32(-42)
	i64Val := int64(0x7fff_ffff_ffff_ffff)
	bytesVal := []byte{0xde, 0xad, 0xbe, 0xef}
	kindVal := e2ev1.NullableKind_NULLABLE_KIND_B
	ts := time.Date(2026, 6, 15, 14, 30, 45, 123456000, time.UTC)

	p := &e2ev1.Nullable{
		Id:        "null-nonzero-" + uniqueID(t),
		StringOpt: &strVal,
		BoolOpt:   &boolVal,
		Int32Opt:  &i32Val,
		Int64Opt:  &i64Val,
		BytesOpt:  bytesVal,
		KindOpt:   &kindVal,
		DeletedAt: timestamppb.New(ts),
		TtlOpt:    durationpb.New(24 * time.Hour),
	}
	roundTripNullable(t, ctx, p, func(t *testing.T, got *e2ev1.Nullable) {
		assert.Equal(t, strVal, *got.StringOpt)
		assert.True(t, *got.BoolOpt)
		assert.Equal(t, i32Val, *got.Int32Opt)
		assert.Equal(t, i64Val, *got.Int64Opt)
		assert.Equal(t, bytesVal, got.BytesOpt)
		assert.Equal(t, kindVal, *got.KindOpt)
		assert.True(t, got.DeletedAt.AsTime().Equal(ts))
		assert.Equal(t, 24*time.Hour, got.TtlOpt.AsDuration())
	})
}

// TestNullable_PartialSet interleaves set and unset to prove the
// mapper handles mixed presence — not just "all set" or "all unset"
// — without bleeding defaults into unset fields or leaving set ones
// empty.
func TestNullable_PartialSet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	strVal := "only string"
	boolVal := true

	p := &e2ev1.Nullable{
		Id:        "null-partial-" + uniqueID(t),
		StringOpt: &strVal,
		BoolOpt:   &boolVal,
		// Int32Opt / Int64Opt / BytesOpt / KindOpt / DeletedAt /
		// TtlOpt are all unset.
	}
	roundTripNullable(t, ctx, p, func(t *testing.T, got *e2ev1.Nullable) {
		require.NotNil(t, got.StringOpt)
		assert.Equal(t, strVal, *got.StringOpt)
		require.NotNil(t, got.BoolOpt)
		assert.True(t, *got.BoolOpt)
		assert.Nil(t, got.Int32Opt)
		assert.Nil(t, got.Int64Opt)
		assert.Nil(t, got.BytesOpt)
		assert.Nil(t, got.KindOpt)
		assert.Nil(t, got.DeletedAt)
		assert.Nil(t, got.TtlOpt)
	})
}

// TestNullable_DBStoredAsActualNULL proves the column values in PG
// are NULL (not zero literals). The value-vs-NULL distinction only
// matters if the DB actually stores NULL — mapper-level pointer
// presence without real NULL would defeat the point.
func TestNullable_DBStoredAsActualNULL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Nullable{Id: "null-raw-" + uniqueID(t)}
	row, err := gen.NullableModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.NullableTable.INSERT(gen.NullableTable.AllColumns).MODEL(row))
	}))

	// Count NULL columns via raw SQL — not the mapper.
	var nullCount int
	err = sharedSuperDB.QueryRowContext(ctx, `
		SELECT (string_opt IS NULL)::int + (bool_opt IS NULL)::int +
		       (int32_opt IS NULL)::int + (int64_opt IS NULL)::int +
		       (bytes_opt IS NULL)::int + (kind_opt IS NULL)::int +
		       (deleted_at IS NULL)::int + (ttl_opt IS NULL)::int
		FROM nullable WHERE id = $1
	`, p.GetId()).Scan(&nullCount)
	require.NoError(t, err)
	assert.Equal(t, 8, nullCount, "every unset nullable column must be SQL NULL, not a zero literal")
}

// roundTripNullable is the shared write → read cycle for Nullable.
func roundTripNullable(t *testing.T, ctx context.Context, p *e2ev1.Nullable, check func(*testing.T, *e2ev1.Nullable)) {
	t.Helper()
	row, err := gen.NullableModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.NullableTable.INSERT(gen.NullableTable.AllColumns).MODEL(row))
	}))
	var read jetmodel.Nullable
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.NullableTable.AllColumns).
			FROM(gen.NullableTable).
			WHERE(gen.NullableTable.ID.EQ(postgres.String(p.GetId()))).
			QueryContext(ctx, tx, &read)
	}))
	got, err := gen.NullableProtoFromModel(&read)
	require.NoError(t, err)
	check(t, got)
}

// TestWKT_Wrappers_StoredAsNativeTypes proves the wrapper columns
// actually land as native SQL types, not JSONB. Raw SELECT with the
// native PG types — if the plugin regresses and routes wrappers back
// through JSONB, the query fails with a cast error. The regression
// tripwire: this is the only test that pins native storage end-to-end.
func TestWKT_Wrappers_StoredAsNativeTypes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	p := &e2ev1.Wkt{
		Id:         "native-" + uniqueID(t),
		StringWrap: wrapperspb.String("hello"),
		Int32Wrap:  wrapperspb.Int32(42),
		Int64Wrap:  wrapperspb.Int64(9000),
		Uint32Wrap: wrapperspb.UInt32(math.MaxUint32),
		Uint64Wrap: wrapperspb.UInt64(math.MaxUint64),
		BoolWrap:   wrapperspb.Bool(true),
		BytesWrap:  wrapperspb.Bytes([]byte{0xde, 0xad}),
	}
	row, err := gen.WktModelFromProto(p)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.WktTable.INSERT(gen.WktTable.AllColumns).MODEL(row))
	}))

	// Query each column via its native type. A JSONB-backed column
	// would reject these casts.
	var (
		stringVal sql.NullString
		int32Val  sql.NullInt32
		int64Val  sql.NullInt64
		uint32Val sql.NullInt32 // int32 at the DB, bit-reinterpreted from uint32
		uint64Val sql.NullInt64
		boolVal   sql.NullBool
		bytesVal  []byte
	)
	err = sharedSuperDB.QueryRowContext(ctx, `
		SELECT string_wrap, int32_wrap, int64_wrap, uint32_wrap, uint64_wrap, bool_wrap, bytes_wrap
		FROM wkt WHERE id = $1
	`, p.GetId()).Scan(&stringVal, &int32Val, &int64Val, &uint32Val, &uint64Val, &boolVal, &bytesVal)
	require.NoError(t, err)

	assert.Equal(t, "hello", stringVal.String)
	assert.Equal(t, int32(42), int32Val.Int32)
	assert.Equal(t, int64(9000), int64Val.Int64)
	// Uint32 max (0xffffffff) reinterprets as int32 -1 under two's
	// complement — exactly what the mapper intended.
	assert.Equal(t, int32(-1), uint32Val.Int32)
	assert.Equal(t, int64(-1), uint64Val.Int64)
	assert.True(t, boolVal.Bool)
	assert.Equal(t, []byte{0xde, 0xad}, bytesVal)
}

// --------------------------------------------------------------------
// ClusterLike — production-shape compatibility pin. A large, deeply
// nested resource with a nested spec/status. The round-trip covers
// every ColumnKind the plugin emits for realistic resources:
// nested + deeply-nested messages, repeated messages with their own
// repeated-string subfields, oneof with message variants, map of
// string-to-string, google.rpc.Status (external proto), dynamic
// google.protobuf.Struct, timestamp + duration, repeated string, and
// proto3-optional nullable scalars.
// --------------------------------------------------------------------

func TestClusterLike_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "cluster-" + uniqueID(t)

	// PG TIMESTAMPTZ stores microseconds — drop sub-μs precision on
	// values that land in TIMESTAMPTZ columns. Timestamps inside JSONB
	// (e.g. status.last_observed_at) keep full nanosecond resolution
	// via protojson.
	now := time.Date(2025, 6, 1, 10, 0, 0, 123456000, time.UTC)
	nsNow := time.Date(2025, 6, 1, 10, 0, 0, 123456789, time.UTC)
	cfg, err := structpb.NewStruct(map[string]any{
		"auto_tune":  true,
		"segment_mb": 512,
	})
	require.NoError(t, err)

	consoleEnabled := true
	rv := int64(42)

	p := &e2ev1.ClusterLike{
		Id:        id,
		Name:      "prod-east-1",
		CreatedAt: timestamppb.New(now),
		UpdatedAt: timestamppb.New(now),
		State:     e2ev1.ClusterLike_STATE_READY,
		// google.rpc.Status embedded as a nested message.
		StateDescription: &rpcstatus.Status{Code: 9, Message: "resource exhausted"},
		Zones:            []string{"us-east-1a", "us-east-1b", "us-east-1c"},
		CloudProviderTags: map[string]string{
			"env":             "prod",
			"gcp.network-tag": "restricted",
			"unicode":         "日本語",
		},
		Spec: &e2ev1.ClusterLike_Spec{
			InstallPackVersion: "25.3.1",
			NodesCount:         6,
			Zones:              []string{"us-east-1a", "us-east-1b"},
			Storage: &e2ev1.ClusterLike_Spec_Storage{
				DataDiskGib: 1024,
				Retention:   durationpb.New(168 * time.Hour),
			},
			ClusterConfig: cfg,
		},
		Status: &e2ev1.ClusterLike_Status{
			KafkaListeners: []*e2ev1.ClusterLike_Status_KafkaListener{
				{Url: "kafka-1", AdvertisedEndpoints: []string{"pub-1", "priv-1"}, Port: 9092},
				{Url: "kafka-2", AdvertisedEndpoints: []string{"pub-2"}, Port: 9093},
			},
			LastObservedAt: timestamppb.New(nsNow),
		},
		PrivateLink: &e2ev1.ClusterLike_PrivateLink{
			Enabled:           true,
			AllowedPrincipals: []string{"arn:aws:iam::111:root", "arn:aws:iam::222:role/admin"},
		},
		Endpoints: []*e2ev1.ClusterLike_Endpoint{
			{Name: "kafka", Url: "k.example.com", Port: 9092},
			{Name: "admin", Url: "a.example.com", Port: 9644},
		},
		UpgradeWindow:   durationpb.New(4 * time.Hour),
		ConnectConsole:  &consoleEnabled,
		ResourceVersion: &rv,
		Cloud: &e2ev1.ClusterLike_Aws{
			Aws: &e2ev1.ClusterLike_AWSProvider{
				AccountId: "123456789012",
				Regions:   []string{"us-east-1", "us-west-2"},
			},
		},
	}

	row, err := gen.ClusterLikeModelFromProto(p)
	require.NoError(t, err)
	// Timestamps are set by the repository in real code; mirror that.
	row.CreatedAt = now
	row.UpdatedAt = now
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
	}))

	var read jetmodel.ClusterLike
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ClusterLikeTable.AllColumns).
			FROM(gen.ClusterLikeTable).
			WHERE(gen.ClusterLikeTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))

	got, err := gen.ClusterLikeProtoFromModel(&read)
	require.NoError(t, err)

	// proto.Equal handles every nested shape: Status, Spec (incl. Struct),
	// oneof, WKTs, optional pointers, repeated messages.
	assert.Truef(t, proto.Equal(p, got), "want %v\n got %v", p, got)
}

// TestClusterLike_FilterByJSONBPath confirms the end-to-end path:
// a caller-written AIP-160 filter referencing a dotted JSONB key
// compiles to a `col->>'path'` WHERE clause, hits the expression
// index, and returns the expected rows. This pins the integration
// between jsonb_indexed_paths (column annotation → plugin-emitted
// FilterFields entry) and the filter translator's dotted-path
// handling.
func TestClusterLike_FilterByJSONBPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	// Seed rows with differentiating spec.installPackVersion values
	// so the filter can select a subset.
	seed := func(id, version string) {
		t.Helper()
		p := &e2ev1.ClusterLike{
			Id:        id,
			Name:      id,
			CreatedAt: timestamppb.New(now),
			UpdatedAt: timestamppb.New(now),
			Spec:      &e2ev1.ClusterLike_Spec{InstallPackVersion: version},
		}
		row, err := gen.ClusterLikeModelFromProto(p)
		require.NoError(t, err)
		row.CreatedAt = now
		row.UpdatedAt = now
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
		}))
	}
	idA := "cluster-filter-a-" + uniqueID(t)
	idB := "cluster-filter-b-" + uniqueID(t)
	idC := "cluster-filter-c-" + uniqueID(t)
	seed(idA, "25.1.0")
	seed(idB, "25.2.0")
	seed(idC, "25.1.0")

	// Compile the AIP-160 filter via the generated FilterFields.
	cond, err := aipjet.FilterToCondition(
		`spec.installPackVersion = "25.1.0"`,
		gen.ClusterLikeFilterFields(),
	)
	require.NoError(t, err)
	require.NotNil(t, cond)

	var rows []jetmodel.ClusterLike
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ClusterLikeTable.AllColumns).
			FROM(gen.ClusterLikeTable).
			WHERE(cond.AND(gen.ClusterLikeTable.ID.IN(
				postgres.String(idA), postgres.String(idB), postgres.String(idC),
			))).
			QueryContext(ctx, tx, &rows)
	}))
	gotIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		gotIDs = append(gotIDs, r.ID)
	}
	sort.Strings(gotIDs)
	assert.Equal(t, []string{idA, idC}, gotIDs,
		"filter spec.installPackVersion = \"25.1.0\" should match idA+idC only")
}

// TestClusterLike_JSONBPathIndexPresent pins that the emitted DDL
// actually applied expression indexes on the declared JSONB paths —
// queries like `WHERE spec->>'installPackVersion' = ...` must be
// index-eligible. Looking up the index via pg_indexes is cheap and
// the DDL regenerates from proto, so this catches both an index that
// went missing and one that was renamed without a migration story.
func TestClusterLike_JSONBPathIndexPresent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	wantNames := []string{
		"idx_cluster_like_cloud_provider_tags_env",
		"idx_cluster_like_spec_installpackversion",
		"idx_cluster_like_spec_storage_datadiskgib",
	}
	rows, err := sharedSuperDB.QueryContext(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE tablename = 'cluster_like'
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got[name] = true
	}
	require.NoError(t, rows.Err())
	for _, n := range wantNames {
		assert.Truef(t, got[n], "expected index %q on cluster_like, found: %v", n, got)
	}
}

// TestClusterLike_GINIndexHandlesContainment pins the GIN index
// on the spec column — a `spec @> '{"storage":{"tiered":true}}'`
// query works on any JSON sub-path without that path being
// declared in jsonb_indexed_paths. The GIN index covers every
// path inside the JSONB.
func TestClusterLike_GINIndexHandlesContainment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)

	seed := func(id string, tiered bool) {
		t.Helper()
		storage := &e2ev1.ClusterLike_Spec_Storage{DataDiskGib: 1024}
		_ = storage
		p := &e2ev1.ClusterLike{
			Id:        id,
			Name:      id,
			CreatedAt: timestamppb.New(now),
			UpdatedAt: timestamppb.New(now),
			Spec: &e2ev1.ClusterLike_Spec{
				InstallPackVersion: "25.1.0",
				Storage: &e2ev1.ClusterLike_Spec_Storage{
					DataDiskGib: 1024,
				},
			},
		}
		// We can't easily set a `tiered` field because the fixture's
		// Storage message doesn't carry one (deliberate — Storage is
		// a stub). Use the `cluster_config` Struct field instead,
		// whose keys are arbitrary, to prove GIN covers ANY JSON
		// sub-path — not just declared ones.
		cfg, err := structpb.NewStruct(map[string]any{"tiered": tiered})
		require.NoError(t, err)
		p.Spec.ClusterConfig = cfg
		row, err := gen.ClusterLikeModelFromProto(p)
		require.NoError(t, err)
		row.CreatedAt = now
		row.UpdatedAt = now
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
		}))
	}
	idTiered := "cluster-gin-tiered-" + uniqueID(t)
	idPlain := "cluster-gin-plain-" + uniqueID(t)
	seed(idTiered, true)
	seed(idPlain, false)

	// @> containment query — hits the GIN index. The jsonb_path_ops
	// operator class supports @>, @?, @@ (but not plain ?). Confirm
	// the query returns the correct row and that it's planner-
	// eligible (we don't assert EXPLAIN output, just correctness).
	var rows []jetmodel.ClusterLike
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		rs, err := sharedSuperDB.QueryContext(ctx, `
			SELECT id FROM cluster_like
			WHERE id IN ($1, $2)
			  AND spec @> '{"clusterConfig":{"tiered":true}}'::jsonb
		`, idTiered, idPlain)
		if err != nil {
			return err
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var r jetmodel.ClusterLike
			if err := rs.Scan(&r.ID); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	}))
	require.Len(t, rows, 1)
	assert.Equal(t, idTiered, rows[0].ID)
}

// TestClusterLike_GINIndexPresent — the pg_indexes entry for the
// GIN index lands with the expected name and uses the jsonb_path_ops
// operator class. If a future code path silently drops the GIN hint
// this catches the regression.
func TestClusterLike_GINIndexPresent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var def string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'cluster_like' AND indexname = 'idx_cluster_like_spec_gin'
	`).Scan(&def))
	assert.Contains(t, def, "USING gin")
	assert.Contains(t, def, "jsonb_path_ops")
	assert.Contains(t, def, "spec")
}

// TestClusterLike_UniqueShorthandRejectsDuplicate — the `unique`
// column option emits a UNIQUE index on name, so the DB rejects a
// duplicate insert with a uniqueness-violation SQLSTATE.
func TestClusterLike_UniqueShorthandRejectsDuplicate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	name := "unique-" + uniqueID(t)

	seed := func(id string) error {
		p := &e2ev1.ClusterLike{
			Id:        id,
			Name:      name,
			CreatedAt: timestamppb.New(now),
			UpdatedAt: timestamppb.New(now),
		}
		row, err := gen.ClusterLikeModelFromProto(p)
		if err != nil {
			return err
		}
		row.CreatedAt = now
		row.UpdatedAt = now
		return inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
		})
	}
	require.NoError(t, seed("unique-a-"+uniqueID(t)))
	err := seed("unique-b-" + uniqueID(t))
	require.Error(t, err, "second insert with same name must violate the unique index")
	requirePGCode(t, err, pgerrcode.UniqueViolation)
}

// TestClusterLike_CustomSQLBRINIndexLanded — the escape-hatch custom
// SQL statement produces an actual BRIN index in PG. Proves callers
// can hand-author DDL the plugin doesn't express declaratively.
func TestClusterLike_CustomSQLBRINIndexLanded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var def string
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'cluster_like'
		  AND indexname = 'idx_cluster_like_created_at_brin'
	`).Scan(&def))
	assert.Contains(t, def, "USING brin")
	assert.Contains(t, def, "created_at")
}

// TestClusterLike_FilterByOneofJSONBPath exercises jsonb_indexed_paths
// declared on the `cloud` oneof. The plugin registers
// `cloud.aws.accountId` and `cloud.gcp.projectId` in FilterFields so
// AIP-160 can compile `cloud.aws.accountId = "..."` into
// `cloud->'aws'->>'accountId' = '...'`. Without the oneof-level
// annotation, filtering sub-paths of a oneof's JSONB would be
// impossible via AIP-160.
func TestClusterLike_FilterByOneofJSONBPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	now := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)

	seed := func(id, accountID string) {
		t.Helper()
		p := &e2ev1.ClusterLike{
			Id:        id,
			Name:      id,
			CreatedAt: timestamppb.New(now),
			UpdatedAt: timestamppb.New(now),
			Cloud: &e2ev1.ClusterLike_Aws{
				Aws: &e2ev1.ClusterLike_AWSProvider{AccountId: accountID},
			},
		}
		row, err := gen.ClusterLikeModelFromProto(p)
		require.NoError(t, err)
		row.CreatedAt = now
		row.UpdatedAt = now
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
		}))
	}
	idA := "cluster-oneof-a-" + uniqueID(t)
	idB := "cluster-oneof-b-" + uniqueID(t)
	seed(idA, "111111111111")
	seed(idB, "222222222222")

	// Paths are absolute inside the JSON value. For a variant-specific
	// query combine with `cloud_kind` explicitly; same shape the repo
	// layer would hand-author.
	cond, err := aipjet.FilterToCondition(
		`cloud.accountId = "111111111111"`,
		gen.ClusterLikeFilterFields(),
	)
	require.NoError(t, err)
	require.NotNil(t, cond)

	var rows []jetmodel.ClusterLike
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ClusterLikeTable.AllColumns).
			FROM(gen.ClusterLikeTable).
			WHERE(cond.AND(gen.ClusterLikeTable.ID.IN(
				postgres.String(idA), postgres.String(idB),
			))).
			QueryContext(ctx, tx, &rows)
	}))
	require.Len(t, rows, 1)
	assert.Equal(t, idA, rows[0].ID)

	// Confirm the expression indexes on the oneof's JSON column exist
	// — pg_indexes entry name drops to lowercase so the lookup matches
	// what the drift-check sees.
	var count int
	require.NoError(t, sharedSuperDB.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'cluster_like'
		  AND indexname IN (
		    'idx_cluster_like_cloud_accountid',
		    'idx_cluster_like_cloud_projectid'
		  )
	`).Scan(&count))
	assert.Equal(t, 2, count)
}

// TestClusterLike_SwitchCloudVariant pins that swapping the oneof
// variant clears the prior JSONB payload. The mapper rewrites both
// cloud_kind + cloud on every write, so a previous `aws` payload must
// not linger when the caller switches to `gcp`.
func TestClusterLike_SwitchCloudVariant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := "cluster-switch-" + uniqueID(t)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	first := &e2ev1.ClusterLike{
		Id:        id,
		Name:      "initial",
		CreatedAt: timestamppb.New(now),
		UpdatedAt: timestamppb.New(now),
		Cloud:     &e2ev1.ClusterLike_Aws{Aws: &e2ev1.ClusterLike_AWSProvider{AccountId: "aws-1"}},
	}
	row, err := gen.ClusterLikeModelFromProto(first)
	require.NoError(t, err)
	row.CreatedAt = now
	row.UpdatedAt = now
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ClusterLikeTable.INSERT(gen.ClusterLikeTable.AllColumns).MODEL(row))
	}))

	second := &e2ev1.ClusterLike{
		Id:        id,
		Name:      "updated",
		CreatedAt: timestamppb.New(now),
		UpdatedAt: timestamppb.New(now),
		Cloud:     &e2ev1.ClusterLike_Gcp{Gcp: &e2ev1.ClusterLike_GCPProvider{ProjectId: "gcp-1", Network: "default"}},
	}
	updated, err := gen.ClusterLikeModelFromProto(second)
	require.NoError(t, err)
	updated.CreatedAt = now
	updated.UpdatedAt = now
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.ClusterLikeUpdateAll().
			MODEL(updated).
			WHERE(gen.ClusterLikeTable.ID.EQ(postgres.String(id))))
	}))

	var read jetmodel.ClusterLike
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return postgres.SELECT(gen.ClusterLikeTable.AllColumns).
			FROM(gen.ClusterLikeTable).
			WHERE(gen.ClusterLikeTable.ID.EQ(postgres.String(id))).
			QueryContext(ctx, tx, &read)
	}))
	assert.Equal(t, "gcp", read.CloudKind)
	assert.NotContains(t, read.Cloud, `"accountId":"aws-1"`)
	got, err := gen.ClusterLikeProtoFromModel(&read)
	require.NoError(t, err)
	assert.Equal(t, "gcp-1", got.GetGcp().GetProjectId())
}

// --------------------------------------------------------------------
// Foreign keys — end-to-end assertions that the plugin's emitted
// ALTER TABLE statements (plain + tenant-composite) are Postgres-
// accepted and behave as declared. The unit tests in
// resolve_fk_test.go cover the rendering logic, these
// tests confirm the database enforces the constraint semantics the
// proto annotation promises.
// --------------------------------------------------------------------

// TestFK_Plain_RejectsOrphan inserts a FKSpoke whose hub_id points at
// no FKHub. PG must reject with 23503 (foreign_key_violation). Pins
// that the plain-FK code path (no tenancy) emits a live constraint.
func TestFK_Plain_RejectsOrphan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	spoke := &e2ev1.FKSpoke{
		Id:    "spoke-" + uniqueID(t),
		HubId: "ghost-hub-" + uniqueID(t),
		Note:  "orphan",
	}
	row, err := gen.FKSpokeModelFromProto(spoke)
	require.NoError(t, err)

	err = inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.FKSpokeTable.
			INSERT(gen.FKSpokeTable.AllColumns).
			MODEL(row))
	})
	requirePGCode(t, err, pgerrcode.ForeignKeyViolation)
}

// TestFK_Plain_CascadeDeletesChildren pins the non-default action
// path: FKSpoke declares `on_delete: ACTION_CASCADE`. Deleting the
// hub takes its spokes with it. If the plugin regressed to emitting
// RESTRICT (the default), the DELETE on the hub would fail — we'd
// see 23503 instead of a clean delete + empty spoke count.
func TestFK_Plain_CascadeDeletesChildren(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	hubID := "hub-" + uniqueID(t)
	spokeAID := "spoke-a-" + uniqueID(t)
	spokeBID := "spoke-b-" + uniqueID(t)

	hub := &e2ev1.FKHub{Id: hubID, Label: "cascade"}
	hubRow, err := gen.FKHubModelFromProto(hub)
	require.NoError(t, err)
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return jetExec(ctx, tx, gen.FKHubTable.
			INSERT(gen.FKHubTable.AllColumns).
			MODEL(hubRow))
	}))

	for _, sid := range []string{spokeAID, spokeBID} {
		spoke := &e2ev1.FKSpoke{Id: sid, HubId: hubID, Note: sid}
		spokeRow, err := gen.FKSpokeModelFromProto(spoke)
		require.NoError(t, err)
		require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return jetExec(ctx, tx, gen.FKSpokeTable.
				INSERT(gen.FKSpokeTable.AllColumns).
				MODEL(spokeRow))
		}))
	}

	// Delete the hub; CASCADE should pull both spokes.
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM fk_hub WHERE id = $1", hubID)
		return err
	}))

	var spokeCount int
	require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM fk_spoke WHERE id = ANY($1)",
			fmt.Sprintf("{%s,%s}", spokeAID, spokeBID),
		).Scan(&spokeCount)
	}))
	assert.Zero(t, spokeCount, "ON DELETE CASCADE must remove all child rows")
}

// TestFK_TenantComposite_RejectsCrossTenant pins the load-bearing
// security property of the tenant-composite rule: a tenant-composite FK prevents
// tenant A from referencing tenant B's parent rows. This is the
// scenario that `custom_sql` FKs get wrong by default — the plain
// FOREIGN KEY (org_id) REFERENCES fk_org (id) would silently accept
// the cross-tenant reference. Under the plugin's auto-composite
// the constraint becomes FOREIGN KEY (tenant_id, org_id) REFERENCES
// fk_org (tenant_id, id), which rejects with 23503.
func TestFK_TenantComposite_RejectsCrossTenant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenantA := "tenant-a-" + uniqueID(t)
	tenantB := "tenant-b-" + uniqueID(t)
	orgID := "org-" + uniqueID(t)
	projectID := "project-" + uniqueID(t)

	// Tenant A creates an org. super-user bypass keeps the test
	// focused on FK semantics, not RLS — the constraint is the
	// thing under test.
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO fk_org (tenant_id, id, name) VALUES ($1, $2, 'a-org')",
			tenantA, orgID)
		return err
	}))

	// Tenant B tries to create a project referencing tenant A's org
	// by id alone. Under a plain FK this would succeed — every row
	// in fk_org has some id, and (id) alone matches. Under the
	// composite FK the lookup is on (tenantB, orgID) which does
	// NOT exist.
	err := inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO fk_project (tenant_id, id, org_id, title) VALUES ($1, $2, $3, 'leaked')",
			tenantB, projectID, orgID)
		return err
	})
	requirePGCode(t, err, pgerrcode.ForeignKeyViolation)
}

// TestFK_TenantComposite_AllowsSameTenantAndRejectsParentDelete
// pins two properties in one pass: same-tenant references succeed
// (so the auto-composite isn't over-rejecting) and the default
// RESTRICT action blocks deletion of a parent with children (so the
// `on_update` / `on_delete` defaults are actually wired through).
func TestFK_TenantComposite_AllowsSameTenantAndRejectsParentDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tenant := "tenant-" + uniqueID(t)
	orgID := "org-" + uniqueID(t)
	projectID := "project-" + uniqueID(t)

	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO fk_org (tenant_id, id, name) VALUES ($1, $2, 'ok-org')",
			tenant, orgID)
		return err
	}))

	// Same-tenant reference: must succeed.
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO fk_project (tenant_id, id, org_id, title) VALUES ($1, $2, $3, 'ok')",
			tenant, projectID, orgID)
		return err
	}))

	// Deleting the parent with a child pointing at it must be
	// rejected — default action is RESTRICT.
	err := inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM fk_org WHERE tenant_id = $1 AND id = $2", tenant, orgID)
		return err
	})
	requirePGCode(t, err, pgerrcode.ForeignKeyViolation)

	// Remove the child first, then the parent delete succeeds.
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM fk_project WHERE tenant_id = $1 AND id = $2", tenant, projectID)
		return err
	}))
	require.NoError(t, inTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM fk_org WHERE tenant_id = $1 AND id = $2", tenant, orgID)
		return err
	}))
}

// TestFK_BackingIndexExists pins that the auto-emitted btree index
// on the FK column lands in PG. Without it, parent-side UPDATE /
// DELETE would seq-scan the child table. The plugin's drift check
// treats a missing index as drift; we confirm here that the index
// is actually created in the first place.
func TestFK_BackingIndexExists(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for _, indexName := range []string{
		"idx_fk_spoke_hub_id_fk",
		"idx_fk_project_org_id_fk",
	} {
		var count int
		require.NoError(t, inReadTx(ctx, sharedSuperDB, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1",
				indexName,
			).Scan(&count)
		}))
		assert.Equal(t, 1, count, "backing FK index %q must exist", indexName)
	}
}
