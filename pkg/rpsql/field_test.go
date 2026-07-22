package rpsql_test

import (
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/rpsql"
)

// tbl mirrors a generated go-jet table with a composite (record) column `addr`
// modelled as a StringColumn.
type probeTable struct {
	postgres.Table
	Addr postgres.ColumnString
	Zip  postgres.ColumnInteger
}

func newProbeTable() *probeTable {
	t := &probeTable{
		Addr: postgres.StringColumn("addr"),
		Zip:  postgres.IntegerColumn("zip"),
	}
	t.Table = postgres.NewTable("public", "probe", "", t.Addr, t.Zip)
	return t
}

func TestField_CompositeAccessSQL(t *testing.T) {
	tbl := newProbeTable()

	stmt := postgres.SELECT(
		rpsql.FieldString(tbl.Addr, "city").AS("city"),
	).FROM(tbl).WHERE(
		rpsql.FieldInt(tbl.Addr, "zip").EQ(postgres.Int(10001)),
	)

	query, args := stmt.Sql()

	// The member access must be parenthesised: (probe.addr).city, never the
	// bare probe.addr.city (which Redpanda SQL parses as table.column).
	if !strings.Contains(query, "(probe.addr).city") {
		t.Errorf("expected parenthesised composite access for projection, got:\n%s", query)
	}
	if !strings.Contains(query, "(probe.addr).zip") {
		t.Errorf("expected parenthesised composite access in WHERE, got:\n%s", query)
	}
	if strings.Contains(query, "probe.addr.city") {
		t.Errorf("bare dotted access must not appear (Redpanda SQL parses it as table.column):\n%s", query)
	}
	if len(args) != 1 || args[0] != int64(10001) {
		t.Errorf("expected single bind arg 10001, got %v", args)
	}
}

func TestField_RejectsNonIdentifier(t *testing.T) {
	tbl := newProbeTable()
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-identifier member name")
		}
	}()
	_ = rpsql.Field(tbl.Addr, "city; DROP TABLE x")
}
