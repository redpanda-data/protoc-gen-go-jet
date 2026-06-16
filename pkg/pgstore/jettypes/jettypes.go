// Package jettypes defines Go types that round-trip specific Postgres
// column shapes which neither go-jet nor `github.com/lib/pq` ship a
// default scanner for. Plugin-generated jet models reference these
// types via `jetgen.ColumnOverrides` so the generated struct fields
// use a type that already knows how to Scan / Value itself.
//
// Keep the set minimal. Every entry is a concession — the more custom
// types land in the model, the more call sites have to know which
// concrete Go type a column exposes. Add one only when the
// alternative (re-keying via pq fallbacks, storing as JSONB) is
// clearly worse.
package jettypes

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// TimestampArray persists a `TIMESTAMPTZ[]` column as a `[]time.Time`
// in Go. `pq.Array([]time.Time{})` fails at scan time with
// `scanning to time.Time is not implemented; only sql.Scanner`
// because pq's array scanner defers to each element's Scanner and
// `time.Time` doesn't implement it. This wrapper scans each element
// via `pq.StringArray` and parses the PG timestamp literal directly,
// then delegates Value to `pq.Array` (which DOES serialise
// `[]time.Time` on the write path — asymmetric but real).
//
// Used by the protoc-gen-go-jet plugin for `repeated
// google.protobuf.Timestamp` fields — the plugin emits TIMESTAMPTZ[]
// in DDL and threads a ColumnOverride to this type through jetgen so
// the jet model field has matching Scan/Value methods.
//
// Times are returned in UTC. PG stores TIMESTAMPTZ in UTC internally
// and returns it in the session timezone; we normalise on scan so
// callers don't depend on the process's TZ configuration.
type TimestampArray []time.Time

// Scan implements [sql.Scanner]. Two wire shapes surface from the
// driver: `[]byte` (pq's default) and `string` (pgx stdlib wrapper,
// occasionally). Both carry a PG array literal like
// `{"2025-04-17 10:20:30.123456+00","2026-01-01 00:00:00+00"}`.
// Scanning via pq.StringArray is the path of least resistance — pq
// already handles quote-escaping and NULL elements, and we just
// reparse each element string as a timestamp.
func (a *TimestampArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var parts pq.StringArray
	if err := parts.Scan(src); err != nil {
		return fmt.Errorf("TimestampArray: scan element strings: %w", err)
	}
	out := make(TimestampArray, 0, len(parts))
	for i, s := range parts {
		t, err := parseTimestampzLiteral(s)
		if err != nil {
			return fmt.Errorf("TimestampArray: element %d %q: %w", i, s, err)
		}
		out = append(out, t)
	}
	*a = out
	return nil
}

// Value implements [driver.Valuer] by delegating to `pq.Array`. pq
// DOES handle `[]time.Time` on the write path — it formats each
// element as an RFC-3339-shaped string inside the array literal.
// A nil receiver encodes as the empty PG array literal `{}` rather
// than SQL NULL, matching the plugin's other array kinds whose
// columns default NOT NULL with `DEFAULT '{}'`.
func (a TimestampArray) Value() (driver.Value, error) {
	if a == nil {
		a = TimestampArray{}
	}
	return pq.Array([]time.Time(a)).Value()
}

// parseTimestampzLiteral parses a PG TIMESTAMPTZ literal as pq
// surfaces it inside an array. PG emits `YYYY-MM-DD HH:MM:SS[.ffffff][±HH[:MM]]`.
// Try microsecond and plain forms in order — the microsecond-precision
// layout wins for the common case, the no-fraction layout catches
// zero-valued times written as `2020-01-01 00:00:00+00`.
//
// Always returns a UTC time so comparing across rows / across
// processes doesn't depend on session timezone.
func parseTimestampzLiteral(s string) (time.Time, error) {
	// PG varies the timezone offset format — `+00`, `+00:00`, or
	// just `+0000` depending on version + client settings. Try a few
	// layouts; fall through to RFC 3339 parsing as a last resort.
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05-07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	// Fallback: swap the space for a `T` and try RFC 3339 — catches
	// drivers that hand us ISO-shaped strings.
	if idx := strings.IndexByte(s, ' '); idx > 0 {
		iso := s[:idx] + "T" + s[idx+1:]
		if t, err := time.Parse(time.RFC3339Nano, iso); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("unrecognised timestamptz layout")
}
