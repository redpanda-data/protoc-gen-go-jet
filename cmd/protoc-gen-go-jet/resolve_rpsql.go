package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	storagev1 "github.com/redpanda-data/protoc-gen-go-jet/gen/go/gojet/v1"
)

// rpsqlReadPlan is the resolved read model for one (gojet.v1.rpsql_read)
// message: a set of typed accessors over the external catalog table the
// message's event stream lands in, bound through pkg/rpsql.CatalogTable.
type rpsqlReadPlan struct {
	Message        *protogen.Message
	ProtoFile      string
	RepoRoot       string
	GoDir          string
	GoPackageName  string
	SymbolPrefix   string // proto message name, e.g. "LlmEvent"
	FilenamePrefix string // lower-cased, e.g. "llmevent"
	Catalog        string
	Table          string
	CTEName        string

	Accessors []rpsqlAccessor
	// SkippedRepeated lists repeated field paths omitted from the read model.
	// rpsql cannot yet read array / composite-array columns.
	SkippedRepeated []string
}

// rpsqlAccessor is one generated method on the read model.
type rpsqlAccessor struct {
	Method string // Go method name, e.g. "ErrorCode"
	Return string // Go return type, e.g. "postgres.ColumnString"
	Body   string // method body expression, e.g. `t.ct.String("model")`
	SQL    string // rendered SQL fragment, for the doc comment
}

// messageRpsqlRead reads the (gojet.v1.rpsql_read) option off a message.
func messageRpsqlRead(m *protogen.Message) (*storagev1.RpsqlRead, bool) {
	opts := m.Desc.Options()
	if opts == nil {
		return nil, false
	}
	if !proto.HasExtension(opts, storagev1.E_RpsqlRead) {
		return nil, false
	}
	rr, ok := proto.GetExtension(opts, storagev1.E_RpsqlRead).(*storagev1.RpsqlRead)
	if !ok || rr == nil {
		return nil, false
	}
	return rr, true
}

// resolveRpsqlRead builds a read-model plan for a message carrying
// (gojet.v1.rpsql_read), or (nil, nil) when the message is not annotated.
func resolveRpsqlRead(f *protogen.File, m *protogen.Message, repoRoot, modulePath string) (*rpsqlReadPlan, error) {
	rr, ok := messageRpsqlRead(m)
	if !ok {
		return nil, nil
	}
	if rr.GetTable() == "" {
		return nil, fmt.Errorf("rpsql_read option missing table")
	}
	if rr.GetCatalog() == "" {
		return nil, fmt.Errorf("rpsql_read option missing catalog")
	}

	protoGoPkg := string(m.GoIdent.GoImportPath)
	if protoGoPkg != modulePath && !strings.HasPrefix(protoGoPkg, modulePath+"/") {
		return nil, fmt.Errorf("proto Go package %q is not under module %q (from go.mod)", protoGoPkg, modulePath)
	}
	protoGoPkgRel := strings.TrimPrefix(protoGoPkg, modulePath+"/")

	cte := rr.GetCteName()
	if cte == "" {
		cte = rr.GetTable()
	}

	plan := &rpsqlReadPlan{
		Message:        m,
		ProtoFile:      f.Desc.Path(),
		RepoRoot:       repoRoot,
		GoDir:          filepath.ToSlash(filepath.Join(protoGoPkgRel, "storage")),
		GoPackageName:  "storage",
		SymbolPrefix:   string(m.Desc.Name()),
		FilenamePrefix: strings.ToLower(string(m.Desc.Name())),
		Catalog:        rr.GetCatalog(),
		Table:          rr.GetTable(),
		CTEName:        cte,
	}

	for _, field := range m.Fields {
		resolveRpsqlField(plan, field)
	}
	if len(plan.Accessors) == 0 {
		return nil, fmt.Errorf("rpsql_read message %s has no readable fields (only repeated/unsupported kinds found)", m.Desc.Name())
	}
	return plan, nil
}

// resolveRpsqlField adds accessor(s) for one top-level field, or records it as
// skipped. Repeated and map fields are skipped (rpsql can't read arrays yet);
// single nested messages recurse into composite-member accessors.
func resolveRpsqlField(plan *rpsqlReadPlan, field *protogen.Field) {
	name := string(field.Desc.Name())
	if field.Desc.IsMap() {
		plan.SkippedRepeated = append(plan.SkippedRepeated, name+" (map)")
		return
	}
	if field.Desc.IsList() {
		plan.SkippedRepeated = append(plan.SkippedRepeated, name+" (repeated)")
		return
	}

	base := fmt.Sprintf("t.ct.String(%q)", name)
	col := fmt.Sprintf("t.ct.%s(%q)", "%s", name) // filled per kind below

	if field.Desc.Kind() == protoreflect.MessageKind || field.Desc.Kind() == protoreflect.GroupKind {
		switch wellKnown(field.Message) {
		case wktTimestamp:
			plan.add(camel(name), "postgres.ColumnTimestampz", fmt.Sprintf(col, "Timestampz"), "(text) timestamptz")
		case wktDuration:
			plan.add(camel(name), "postgres.ColumnInteger", fmt.Sprintf(col, "Int"), "bigint (ns)")
		case wktWrapper:
			addScalarColumn(plan, camel(name), field.Message.Fields[0].Desc.Kind(), fmt.Sprintf(col, "%s"))
		default:
			// Composite: recurse into the message's members. The base
			// expression is the column itself; members drill in via (col).member.
			addRpsqlComposite(plan, base, camel(name), field.Message)
		}
		return
	}

	if field.Desc.Kind() == protoreflect.EnumKind {
		plan.add(camel(name), "postgres.ColumnString", fmt.Sprintf(col, "String"), "text (enum name)")
		return
	}
	addScalarColumn(plan, camel(name), field.Desc.Kind(), col)
}

// addScalarColumn adds a top-level scalar accessor using a CatalogTable column
// helper (String/Int/Bool/Float). colTmpl carries the field name and a single
// %s for the helper name.
func addScalarColumn(plan *rpsqlReadPlan, method string, kind protoreflect.Kind, colTmpl string) {
	switch scalarClass(kind) {
	case classString:
		plan.add(method, "postgres.ColumnString", fmt.Sprintf(colTmpl, "String"), "text")
	case classInt:
		plan.add(method, "postgres.ColumnInteger", fmt.Sprintf(colTmpl, "Int"), "integer/bigint")
	case classBool:
		plan.add(method, "postgres.ColumnBool", fmt.Sprintf(colTmpl, "Bool"), "boolean")
	case classFloat:
		plan.add(method, "postgres.ColumnFloat", fmt.Sprintf(colTmpl, "Float"), "real/double")
	default:
		// bytes and anything unmapped: skip silently — not readable as a
		// typed scalar on the rpsql read side.
	}
}

// addRpsqlComposite recurses into a composite message, emitting a member
// accessor per readable field. baseExpr is the Go expression for the composite
// column; methodPrefix accumulates the Go method-name path.
func addRpsqlComposite(plan *rpsqlReadPlan, baseExpr, methodPrefix string, msg *protogen.Message) {
	for _, mf := range msg.Fields {
		name := string(mf.Desc.Name())
		if mf.Desc.IsMap() || mf.Desc.IsList() {
			plan.SkippedRepeated = append(plan.SkippedRepeated, methodPrefix+"."+name+" (repeated)")
			continue
		}
		method := methodPrefix + camel(name)
		if mf.Desc.Kind() == protoreflect.MessageKind || mf.Desc.Kind() == protoreflect.GroupKind {
			switch wellKnown(mf.Message) {
			case wktTimestamp:
				plan.add(method, "postgres.TimestampzExpression", fmt.Sprintf("rpsql.FieldTimestampz(%s, %q)", baseExpr, name), "timestamptz")
			case wktDuration:
				plan.add(method, "postgres.IntegerExpression", fmt.Sprintf("rpsql.FieldInt(%s, %q)", baseExpr, name), "bigint (ns)")
			case wktWrapper:
				addScalarMember(plan, method, mf.Message.Fields[0].Desc.Kind(), baseExpr, name)
			default:
				addRpsqlComposite(plan, fmt.Sprintf("rpsql.Field(%s, %q)", baseExpr, name), method, mf.Message)
			}
			continue
		}
		if mf.Desc.Kind() == protoreflect.EnumKind {
			plan.add(method, "postgres.StringExpression", fmt.Sprintf("rpsql.FieldString(%s, %q)", baseExpr, name), "text (enum name)")
			continue
		}
		addScalarMember(plan, method, mf.Desc.Kind(), baseExpr, name)
	}
}

// addScalarMember adds a composite-member accessor via a typed rpsql.Field*.
func addScalarMember(plan *rpsqlReadPlan, method string, kind protoreflect.Kind, baseExpr, name string) {
	switch scalarClass(kind) {
	case classString:
		plan.add(method, "postgres.StringExpression", fmt.Sprintf("rpsql.FieldString(%s, %q)", baseExpr, name), "text")
	case classInt:
		plan.add(method, "postgres.IntegerExpression", fmt.Sprintf("rpsql.FieldInt(%s, %q)", baseExpr, name), "integer/bigint")
	case classBool:
		plan.add(method, "postgres.BoolExpression", fmt.Sprintf("rpsql.FieldBool(%s, %q)", baseExpr, name), "boolean")
	case classFloat:
		plan.add(method, "postgres.FloatExpression", fmt.Sprintf("rpsql.FieldFloat(%s, %q)", baseExpr, name), "real/double")
	}
}

func (p *rpsqlReadPlan) add(method, ret, body, sql string) {
	p.Accessors = append(p.Accessors, rpsqlAccessor{Method: method, Return: ret, Body: body, SQL: sql})
}

type wkt int

const (
	wktNone wkt = iota
	wktTimestamp
	wktDuration
	wktWrapper
)

// wellKnown classifies a message field as a well-known type the read side maps
// to a scalar column rather than a composite.
func wellKnown(m *protogen.Message) wkt {
	switch m.Desc.FullName() {
	case "google.protobuf.Timestamp":
		return wktTimestamp
	case "google.protobuf.Duration":
		return wktDuration
	case "google.protobuf.StringValue", "google.protobuf.BoolValue",
		"google.protobuf.Int32Value", "google.protobuf.Int64Value",
		"google.protobuf.UInt32Value", "google.protobuf.UInt64Value",
		"google.protobuf.FloatValue", "google.protobuf.DoubleValue",
		"google.protobuf.BytesValue":
		return wktWrapper
	default:
		return wktNone
	}
}

type scalarKind int

const (
	classUnsupported scalarKind = iota
	classString
	classInt
	classBool
	classFloat
)

func scalarClass(k protoreflect.Kind) scalarKind {
	switch k {
	case protoreflect.StringKind:
		return classString
	case protoreflect.BoolKind:
		return classBool
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return classInt
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return classFloat
	default:
		return classUnsupported
	}
}

// camel converts a snake_case proto field name to a PascalCase Go identifier
// segment (agent_id -> AgentId, input_tokens -> InputTokens).
func camel(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}
