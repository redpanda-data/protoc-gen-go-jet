package main

import (
	"path/filepath"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// isUnsignedInt32Kind reports proto3 kinds that `protoc-gen-go` renders
// as the Go `uint32` type. The plugin's jet model stores them as
// `int32`, so the mapper has to insert an explicit conversion both
// ways — plain assignment would fail to compile.
func isUnsignedInt32Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Uint32Kind || k == protoreflect.Fixed32Kind
}

// isUnsignedInt64Kind is the uint64 counterpart (Uint64 and Fixed64).
func isUnsignedInt64Kind(k protoreflect.Kind) bool {
	return k == protoreflect.Uint64Kind || k == protoreflect.Fixed64Kind
}

// Import paths used repeatedly by the mapper template. Centralised so a
// typo can't produce a "works on 4 of 6 sites" regression.
const (
	pqPkg        = "github.com/lib/pq"
	protojsonPkg = "google.golang.org/protobuf/encoding/protojson"
	encodingPkg  = "encoding/json"
	fmtPkg       = "fmt"
	strconvPkg   = "strconv"
	timestampPkg = "google.golang.org/protobuf/types/known/timestamppb"
	durationPkg  = "google.golang.org/protobuf/types/known/durationpb"
	wrappersPkg  = "google.golang.org/protobuf/types/known/wrapperspb"
	timePkg      = "time"
	jettypesPkg  = "github.com/redpanda-data/protoc-gen-go-jet/pkg/pgstore/jettypes"
)

// qual returns the file-qualified form of a (pkg, name) pair, which is
// what protogen expects you to emit into generated code.
func qual(g *protogen.GeneratedFile, pkg, name string) string {
	return g.QualifiedGoIdent(protogen.GoIdent{GoImportPath: protogen.GoImportPath(pkg), GoName: name})
}

// writeRepeatedPrimitiveModelAssign emits the proto → model branch for
// repeated scalar kinds that target a pq.<pqType> column. nil-slice
// from proto3 is coerced to an empty pq array — same NOT-NULL-
// preserving guard as KindRepeatedText. When castElem is non-empty
// (uint32 → int32, uint64 → int64), each element is converted to
// preserve the two's-complement round-trip.
func writeRepeatedPrimitiveModelAssign(g *protogen.GeneratedFile, c ColumnPlan, src, pqType, castElem string) {
	pqIdent := qual(g, pqPkg, pqType)
	if castElem == "" {
		g.P("    if v := ", src, "; v != nil {")
		g.P("        m.", c.JetFieldName, " = ", pqIdent, "(v)")
		g.P("    } else {")
		g.P("        m.", c.JetFieldName, " = ", pqIdent, "{}")
		g.P("    }")
		return
	}
	g.P("    {")
	g.P("        items := ", src)
	g.P("        arr := make(", pqIdent, ", len(items))")
	g.P("        for i, v := range items { arr[i] = ", castElem, "(v) }")
	g.P("        m.", c.JetFieldName, " = arr")
	g.P("    }")
}

// goMapKeyType returns the Go type protoc-gen-go renders for a proto map
// key of the given Kind. Proto3 allows string, bool, and any integer
// kind; signed varints / fixed / zigzag collapse to plain int32/int64
// at the Go layer, unsigned kinds to uint32/uint64.
func goMapKeyType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	}
	return ""
}

// emitKeyToString writes an expression that converts a map key variable
// of the given proto Kind to a JSON string key. Keeps the encoding
// deterministic across key types — every stored JSONB object uses the
// base-10 representation for integers, "true"/"false" for bool, and the
// string itself for string keys (which is what json.Marshal would
// produce anyway, but doing it explicitly avoids depending on stdlib
// quirks like bool-key support).
func emitKeyToString(g *protogen.GeneratedFile, k protoreflect.Kind) string {
	// The generated range variable is always `k` — every caller emits
	// `for k, v := range src` immediately above.
	const src = "k"
	switch k {
	case protoreflect.StringKind:
		return src
	case protoreflect.BoolKind:
		return qual(g, strconvPkg, "FormatBool") + "(" + src + ")"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return qual(g, strconvPkg, "FormatInt") + "(int64(" + src + "), 10)"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return qual(g, strconvPkg, "FormatUint") + "(uint64(" + src + "), 10)"
	}
	// Unreachable — inferMapEncoding rejects any other key kind.
	return src
}

// goMapScalarValueType returns the Go value type protoc-gen-go renders
// for a proto map scalar value. Mirrors the scalar switch in
// inferFieldEncoding — this is the Go type we round-trip through
// encoding/json.
func goMapScalarValueType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.BytesKind:
		return "[]byte"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	}
	return ""
}

// emitStringToKey writes the parse-and-assign sequence that turns a
// string map key back into the proto Go key type. `src` is the string
// expression; `dst` is the lvalue that receives the parsed key. On
// parse failure the generated code returns an error — we deliberately
// don't try to recover, because a mangled key means the JSONB payload
// is corrupt and silent discard would mask the bug.
func emitStringToKey(g *protogen.GeneratedFile, k protoreflect.Kind) {
	// Callers always decode from `ks` into `rk` and return `nil, err`
	// on parse failure — every caller is inside the same JSONB-map read
	// loop shape. Keeping these as parameters was cosmetic.
	const (
		dst    = "rk"
		src    = "ks"
		errRet = "nil"
	)
	switch k {
	case protoreflect.StringKind:
		g.P("        ", dst, " = ", src)
	case protoreflect.BoolKind:
		g.P("        parsedKey, err := ", qual(g, strconvPkg, "ParseBool"), "(", src, ")")
		g.P("        if err != nil { return ", errRet, ", ", qual(g, fmtPkg, "Errorf"), "(\"parse bool key %q: %w\", ", src, ", err) }")
		g.P("        ", dst, " = parsedKey")
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		g.P("        parsedKey, err := ", qual(g, strconvPkg, "ParseInt"), "(", src, ", 10, 32)")
		g.P("        if err != nil { return ", errRet, ", ", qual(g, fmtPkg, "Errorf"), "(\"parse int32 key %q: %w\", ", src, ", err) }")
		g.P("        ", dst, " = int32(parsedKey)")
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		g.P("        parsedKey, err := ", qual(g, strconvPkg, "ParseInt"), "(", src, ", 10, 64)")
		g.P("        if err != nil { return ", errRet, ", ", qual(g, fmtPkg, "Errorf"), "(\"parse int64 key %q: %w\", ", src, ", err) }")
		g.P("        ", dst, " = parsedKey")
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		g.P("        parsedKey, err := ", qual(g, strconvPkg, "ParseUint"), "(", src, ", 10, 32)")
		g.P("        if err != nil { return ", errRet, ", ", qual(g, fmtPkg, "Errorf"), "(\"parse uint32 key %q: %w\", ", src, ", err) }")
		g.P("        ", dst, " = uint32(parsedKey)")
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		g.P("        parsedKey, err := ", qual(g, strconvPkg, "ParseUint"), "(", src, ", 10, 64)")
		g.P("        if err != nil { return ", errRet, ", ", qual(g, fmtPkg, "Errorf"), "(\"parse uint64 key %q: %w\", ", src, ", err) }")
		g.P("        ", dst, " = uint64(parsedKey)")
	}
}

// emitMapper writes mapper.go — pure proto <-> go-jet-model conversion.
//
// The generated package imports:
//   - the proto Go package (for the resource type)
//   - the go-jet model package (for the row struct)
//   - protojson, timestamppb, time, encoding/json
//
// tenantID / timestamps are supplied by the repository (not the mapper).
// The mapper only handles field-by-field translation.
func emitMapper(gen *protogen.Plugin, p *ResourcePlan) {
	relPath := filepath.Join(p.GoDir, p.FilenamePrefix+"_mapper.go")
	g := gen.NewGeneratedFile(relPath, protogen.GoImportPath(p.GoImportPath))

	g.P("// Code generated by protoc-gen-go-jet. DO NOT EDIT.")
	g.P("// Source: ", p.ProtoFile)
	g.P()
	g.P("package ", p.GoPackageName)
	g.P()

	protoIdent := p.ProtoGoIdent
	modelIdent := protogen.GoIdent{
		GoImportPath: protogen.GoImportPath(p.JetModelImport),
		GoName:       p.JetStructName,
	}

	writeModelFromProto(g, p, protoIdent, modelIdent)
	writeProtoFromModel(g, p, protoIdent, modelIdent)
}

func writeModelFromProto(g *protogen.GeneratedFile, p *ResourcePlan, protoIdent, modelIdent protogen.GoIdent) {
	g.P("// ", p.SymbolPrefix, "ModelFromProto converts the API proto into a go-jet row.")
	g.P("//")
	g.P("// tenant_id / user_id / timestamps are set by the repository, not here —")
	g.P("// they are storage concerns supplied at the SessionRunner level.")
	g.P("func ", p.SymbolPrefix, "ModelFromProto(p *", protoIdent, ") (", modelIdent, ", error) {")
	g.P("    m := ", modelIdent, "{}")
	for _, c := range p.Columns {
		// Non-nullable Timestamp columns are repo-managed (created_at
		// gets now() on INSERT, updated_at gets now() on every UPDATE)
		// — the mapper leaves them alone. Nullable timestamps (e.g.
		// soft-delete `deleted_at`) are user-managed and go through
		// the pointer-aware branch like any other nullable kind.
		//
		// Generated columns are PG-managed: the database computes the
		// value on every INSERT / UPDATE, and PG rejects any client-
		// provided value. Skip the mapper write so a regenerated row
		// passing through proto→model→INSERT doesn't trip the SQL
		// "cannot insert into column ... GENERATED ALWAYS" error.
		if c.Synthesized || (c.Kind == KindTimestamp && !c.Nullable) || c.GeneratedExpr != "" {
			continue
		}
		writeModelAssignFromProto(g, c)
	}
	// Oneofs
	for _, oc := range p.OneofColumns {
		writeOneofModelAssign(g, oc)
	}
	g.P("    return m, nil")
	g.P("}")
	g.P()
}

func writeModelAssignFromProto(g *protogen.GeneratedFile, c ColumnPlan) {
	src := "p.Get" + c.Field.GoName + "()"
	if c.Nullable {
		writeNullableModelAssign(g, c, src)
		return
	}
	switch c.Kind {
	case KindScalar:
		// KindScalar covers a handful of Go-type mismatches the plain
		// assignment doesn't handle:
		//
		//   []byte with a nil value — proto3 returns nil for an unset
		//   bytes field; pq/go-jet writes nil as SQL NULL, tripping
		//   NOT NULL on the column. Coerce to non-nil empty.
		//
		//   uint32 / fixed32 / uint64 / fixed64 — protoc-gen-go renders
		//   these as uint* Go types; the jet model stores them as int*.
		//   The bit pattern round-trips losslessly under two's complement,
		//   so a static conversion is safe. Without it, the file won't
		//   compile.
		switch {
		case c.JetGoType == "[]byte":
			g.P("    if v := ", src, "; v != nil {")
			g.P("        m.", c.JetFieldName, " = v")
			g.P("    } else {")
			g.P("        m.", c.JetFieldName, " = []byte{}")
			g.P("    }")
		case c.Field != nil && isUnsignedInt32Kind(c.Field.Desc.Kind()):
			g.P("    m.", c.JetFieldName, " = int32(", src, ")")
		case c.Field != nil && isUnsignedInt64Kind(c.Field.Desc.Kind()):
			g.P("    m.", c.JetFieldName, " = int64(", src, ")")
		default:
			g.P("    m.", c.JetFieldName, " = ", src)
		}
	case KindDuration:
		// proto Duration -> nanoseconds (int64). AsDuration handles a
		// nil receiver, yielding zero.
		g.P("    m.", c.JetFieldName, " = int64(", src, ".AsDuration())")
	case KindEnumAsText:
		g.P("    m.", c.JetFieldName, " = ", src, ".String()")
	case KindRepeatedText:
		// proto3 unset repeated fields are nil, not empty. pq.StringArray
		// encodes a nil receiver as SQL NULL, which violates NOT NULL on
		// DEFAULT '{}' columns. Force non-nil so the empty case always
		// maps to the empty array literal.
		pqIdent := qual(g, pqPkg, "StringArray")
		g.P("    if v := ", src, "; v != nil {")
		g.P("        m.", c.JetFieldName, " = ", pqIdent, "(v)")
		g.P("    } else {")
		g.P("        m.", c.JetFieldName, " = ", pqIdent, "{}")
		g.P("    }")
	case KindRepeatedEnum:
		pqIdent := qual(g, pqPkg, "StringArray")
		g.P("    {")
		g.P("        items := ", src)
		g.P("        names := make(", pqIdent, ", 0, len(items))")
		g.P("        for _, v := range items { names = append(names, v.String()) }")
		g.P("        m.", c.JetFieldName, " = names")
		g.P("    }")
	case KindRepeatedBool:
		writeRepeatedPrimitiveModelAssign(g, c, src, "BoolArray", "")
	case KindRepeatedInt32:
		// uint32 / fixed32 need element-wise int32 conversion for the
		// same two's-complement reinterpretation bare uint32 scalars
		// use. Signed int32 kinds assign directly.
		castElem := ""
		if isUnsignedInt32Kind(c.Field.Desc.Kind()) {
			castElem = "int32"
		}
		writeRepeatedPrimitiveModelAssign(g, c, src, "Int32Array", castElem)
	case KindRepeatedInt64:
		castElem := ""
		if isUnsignedInt64Kind(c.Field.Desc.Kind()) {
			castElem = "int64"
		}
		writeRepeatedPrimitiveModelAssign(g, c, src, "Int64Array", castElem)
	case KindRepeatedFloat32:
		writeRepeatedPrimitiveModelAssign(g, c, src, "Float32Array", "")
	case KindRepeatedFloat64:
		writeRepeatedPrimitiveModelAssign(g, c, src, "Float64Array", "")
	case KindRepeatedTimestamp:
		// []*timestamppb.Timestamp -> jettypes.TimestampArray (a
		// []time.Time named type that knows how to Scan / Value a
		// TIMESTAMPTZ[] column via pq.Array). AsTime() handles nil
		// receivers (returns zero time), so the mapper doesn't need
		// a nil guard on individual elements.
		arrIdent := qual(g, jettypesPkg, "TimestampArray")
		g.P("    {")
		g.P("        items := ", src)
		g.P("        arr := make(", arrIdent, ", len(items))")
		g.P("        for i, v := range items { arr[i] = v.AsTime() }")
		g.P("        m.", c.JetFieldName, " = arr")
		g.P("    }")
	case KindRepeatedDuration:
		// []*durationpb.Duration -> pq.Int64Array of nanoseconds.
		// AsDuration() handles nil receiver (yields zero), same trick as
		// the single Duration path.
		pqIdent := qual(g, pqPkg, "Int64Array")
		g.P("    {")
		g.P("        items := ", src)
		g.P("        arr := make(", pqIdent, ", len(items))")
		g.P("        for i, v := range items { arr[i] = int64(v.AsDuration()) }")
		g.P("        m.", c.JetFieldName, " = arr")
		g.P("    }")
	case KindJSONBProto:
		// Nested message → protojson, assigned as string. go-jet's
		// default model type for JSONB is string; pgx serialises
		// string → JSONB as text, which is what PG expects.
		g.P("    if v := ", src, "; v != nil {")
		g.P("        raw, err := ", qual(g, protojsonPkg, "Marshal"), "(v)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(raw)")
		g.P("    } else {")
		g.P("        m.", c.JetFieldName, " = \"{}\"")
		g.P("    }")
	case KindJSONBProtoList:
		// []*Message → JSON array of protojson-marshaled items.
		// Build a []json.RawMessage and let encoding/json handle the
		// array framing (brackets, commas). The protojson output is
		// already valid JSON, so wrapping it in RawMessage keeps the
		// serialiser from double-escaping and guarantees correct array
		// shape even if protojson ever emits whitespace in the future.
		g.P("    {")
		g.P("        items := ", src)
		g.P("        raws := make([]", qual(g, encodingPkg, "RawMessage"), ", 0, len(items))")
		g.P("        for _, item := range items {")
		g.P("            raw, err := ", qual(g, protojsonPkg, "Marshal"), "(item)")
		g.P("            if err != nil { return m, err }")
		g.P("            raws = append(raws, raw)")
		g.P("        }")
		g.P("        buf, err := ", qual(g, encodingPkg, "Marshal"), "(raws)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(buf)")
		g.P("    }")
	case KindJSONBStrMap:
		g.P("    if v := ", src, "; v != nil {")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(v)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(raw)")
		g.P("    } else {")
		g.P("        m.", c.JetFieldName, " = \"{}\"")
		g.P("    }")
	case KindJSONBMapScalar:
		// map<K, scalar V> → JSONB object. Always re-key to map[string]V
		// so the stored JSON shape is independent of stdlib json's
		// per-key-type quirks (bool keys aren't supported at all by
		// json.Marshal). The scalar V type survives untouched: strings,
		// ints, bools, and []byte (auto-base64) all round-trip through
		// encoding/json natively. A nil proto-side map serialises as the
		// empty JSON object so the column stays non-nil.
		valGo := goMapScalarValueType(c.MapValueKind)
		g.P("    {")
		g.P("        src := ", src)
		g.P("        dst := make(map[string]", valGo, ", len(src))")
		g.P("        for k, v := range src {")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = v")
		g.P("        }")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(raw)")
		g.P("    }")
	case KindJSONBMapEnum:
		// map<K, SomeEnum> → JSONB object. Re-key to string and
		// stringify each enum value by name; the column holds a
		// self-describing object like {"k": "ENUM_NAME"}. Matches
		// KindEnumAsText for bare enum scalars.
		g.P("    {")
		g.P("        src := ", src)
		g.P("        dst := make(map[string]string, len(src))")
		g.P("        for k, v := range src {")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = v.String()")
		g.P("        }")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(raw)")
		g.P("    }")
	case KindJSONBMapMessage:
		// map<K, message V> → JSONB object, per-value protojson wrapped
		// in json.RawMessage to frame the outer object. Mirrors the
		// KindJSONBProtoList pattern: protojson owns each value's
		// serialisation, encoding/json owns the object framing. Key
		// stringification is explicit so we never rely on stdlib's
		// TextMarshaler fallback (which doesn't cover bool keys).
		g.P("    {")
		g.P("        src := ", src)
		g.P("        dst := make(map[string]", qual(g, encodingPkg, "RawMessage"), ", len(src))")
		g.P("        for k, v := range src {")
		g.P("            raw, err := ", qual(g, protojsonPkg, "Marshal"), "(v)")
		g.P("            if err != nil { return m, err }")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = raw")
		g.P("        }")
		g.P("        buf, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        m.", c.JetFieldName, " = string(buf)")
		g.P("    }")
	}
}

func writeOneofModelAssign(g *protogen.GeneratedFile, oc OneofColumnPlan) {
	g.P("    switch v := p.Get", oc.Oneof.GoName, "().(type) {")
	for _, vr := range oc.Variants {
		wrap := vr.Field.GoIdent
		g.P("    case *", wrap, ":")
		g.P("        m.", oc.JetKindField, " = \"", vr.VariantName, "\"")
		switch vr.Field.Desc.Kind() {
		case protoreflect.MessageKind:
			// Message variants round-trip via protojson so unknown
			// fields / proto3-well-known-type quirks survive.
			g.P("        raw, err := ", qual(g, protojsonPkg, "Marshal"), "(v.", vr.Field.GoName, ")")
			g.P("        if err != nil { return m, err }")
			g.P("        m.", oc.JetJSONField, " = string(raw)")
		case protoreflect.EnumKind:
			// Enum variants store the enum's String() name, matching
			// how bare enum scalars persist (KindEnumAsText).
			g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(v.", vr.Field.GoName, ".String())")
			g.P("        if err != nil { return m, err }")
			g.P("        m.", oc.JetJSONField, " = string(raw)")
		default:
			// Scalars round-trip via stdlib json: string/bool/number/
			// []byte all have native encoders. Bytes base64-encodes
			// automatically.
			g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(v.", vr.Field.GoName, ")")
			g.P("        if err != nil { return m, err }")
			g.P("        m.", oc.JetJSONField, " = string(raw)")
		}
	}
	g.P("    case nil:")
	if oc.Optional {
		g.P("        m.", oc.JetKindField, " = \"\"")
		g.P("        m.", oc.JetJSONField, " = \"{}\"")
	} else {
		g.P("        return m, ", qual(g, fmtPkg, "Errorf"), "(\"", oc.BaseName, " is required\")")
	}
	g.P("    default:")
	g.P("        return m, ", qual(g, fmtPkg, "Errorf"), "(\"unknown ", oc.BaseName, " variant %T\", v)")
	g.P("    }")
}

func writeProtoFromModel(g *protogen.GeneratedFile, p *ResourcePlan, protoIdent, modelIdent protogen.GoIdent) {
	g.P("// ", p.SymbolPrefix, "ProtoFromModel converts a go-jet row into the API proto.")
	g.P("func ", p.SymbolPrefix, "ProtoFromModel(m *", modelIdent, ") (*", protoIdent, ", error) {")
	g.P("    if m == nil { return nil, nil }")
	g.P("    out := &", protoIdent, "{}")
	for _, c := range p.Columns {
		if c.Synthesized {
			continue
		}
		writeProtoAssignFromModel(g, c)
	}
	for _, oc := range p.OneofColumns {
		writeOneofProtoAssign(g, oc)
	}
	g.P("    return out, nil")
	g.P("}")
}

// writeNullableModelAssign emits the proto → model branch for a
// nullable column. Every nullable jet model field is a pointer — jet
// always pointerises nullable columns, including `*[]byte` for BYTEA.
// Presence is read off the proto struct's pointer field directly
// (API_OPEN doesn't emit HasX helpers; the field itself is the pointer
// for proto3-optional scalars / enums / messages).
//
// bytes is the one exception: protoc-gen-go leaves proto3-optional
// bytes as `[]byte`, not `*[]byte`. A nil slice means unset; we
// promote to `*[]byte` by taking its address when non-nil.
func writeNullableModelAssign(g *protogen.GeneratedFile, c ColumnPlan, src string) {
	fieldRef := "p." + c.Field.GoName
	if c.WrapperCtor != "" {
		writeWrapperModelAssign(g, c, src)
		return
	}
	switch c.Kind {
	case KindScalar:
		if c.JetGoType == "*[]byte" {
			// Optional bytes: []byte at the proto level, *[]byte at jet.
			// Nil slice signals unset on the proto side.
			g.P("    if ", fieldRef, " != nil {")
			g.P("        v := ", src)
			g.P("        m.", c.JetFieldName, " = &v")
			g.P("    }")
			return
		}
		g.P("    if ", fieldRef, " != nil {")
		switch {
		case isUnsignedInt32Kind(c.Field.Desc.Kind()):
			g.P("        v := int32(", src, ")")
		case isUnsignedInt64Kind(c.Field.Desc.Kind()):
			g.P("        v := int64(", src, ")")
		default:
			g.P("        v := ", src)
		}
		g.P("        m.", c.JetFieldName, " = &v")
		g.P("    }")
	case KindTimestamp:
		g.P("    if t := ", src, "; t != nil {")
		g.P("        v := t.AsTime()")
		g.P("        m.", c.JetFieldName, " = &v")
		g.P("    }")
	case KindDuration:
		g.P("    if ", fieldRef, " != nil {")
		g.P("        v := int64(", src, ".AsDuration())")
		g.P("        m.", c.JetFieldName, " = &v")
		g.P("    }")
	case KindEnumAsText:
		g.P("    if ", fieldRef, " != nil {")
		g.P("        v := ", src, ".String()")
		g.P("        m.", c.JetFieldName, " = &v")
		g.P("    }")
	case KindRepeatedText:
		// Proto3 nil slice → SQL NULL; non-nil (including empty) slice →
		// non-nil pointer so empty-vs-absent survives.
		pqIdent := qual(g, pqPkg, "StringArray")
		g.P("    if v := ", src, "; v != nil {")
		g.P("        arr := ", pqIdent, "(v)")
		g.P("        m.", c.JetFieldName, " = &arr")
		g.P("    }")
	case KindRepeatedEnum:
		pqIdent := qual(g, pqPkg, "StringArray")
		g.P("    if v := ", src, "; v != nil {")
		g.P("        names := make(", pqIdent, ", 0, len(v))")
		g.P("        for _, e := range v { names = append(names, e.String()) }")
		g.P("        m.", c.JetFieldName, " = &names")
		g.P("    }")
	case KindRepeatedBool:
		writeNullableRepeatedPrimitiveAssign(g, c, src, "BoolArray", "")
	case KindRepeatedInt32:
		castElem := ""
		if isUnsignedInt32Kind(c.Field.Desc.Kind()) {
			castElem = "int32"
		}
		writeNullableRepeatedPrimitiveAssign(g, c, src, "Int32Array", castElem)
	case KindRepeatedInt64:
		castElem := ""
		if isUnsignedInt64Kind(c.Field.Desc.Kind()) {
			castElem = "int64"
		}
		writeNullableRepeatedPrimitiveAssign(g, c, src, "Int64Array", castElem)
	case KindRepeatedFloat32:
		writeNullableRepeatedPrimitiveAssign(g, c, src, "Float32Array", "")
	case KindRepeatedFloat64:
		writeNullableRepeatedPrimitiveAssign(g, c, src, "Float64Array", "")
	case KindRepeatedTimestamp:
		arrIdent := qual(g, jettypesPkg, "TimestampArray")
		g.P("    if v := ", src, "; v != nil {")
		g.P("        arr := make(", arrIdent, ", len(v))")
		g.P("        for i, t := range v { arr[i] = t.AsTime() }")
		g.P("        m.", c.JetFieldName, " = &arr")
		g.P("    }")
	case KindRepeatedDuration:
		pqIdent := qual(g, pqPkg, "Int64Array")
		g.P("    if v := ", src, "; v != nil {")
		g.P("        arr := make(", pqIdent, ", len(v))")
		g.P("        for i, d := range v { arr[i] = int64(d.AsDuration()) }")
		g.P("        m.", c.JetFieldName, " = &arr")
		g.P("    }")
	case KindJSONBProto:
		// Single nested message. nil -> leave the *string nil -> SQL NULL
		// (absent); a present message (even all-default) -> protojson string,
		// so "{}" round-trips as a present-but-empty message, distinct from NULL.
		g.P("    if v := ", src, "; v != nil {")
		g.P("        raw, err := ", qual(g, protojsonPkg, "Marshal"), "(v)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(raw)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	case KindJSONBProtoList:
		g.P("    if v := ", src, "; v != nil {")
		g.P("        raws := make([]", qual(g, encodingPkg, "RawMessage"), ", 0, len(v))")
		g.P("        for _, item := range v {")
		g.P("            raw, err := ", qual(g, protojsonPkg, "Marshal"), "(item)")
		g.P("            if err != nil { return m, err }")
		g.P("            raws = append(raws, raw)")
		g.P("        }")
		g.P("        buf, err := ", qual(g, encodingPkg, "Marshal"), "(raws)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(buf)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	case KindJSONBStrMap:
		g.P("    if v := ", src, "; v != nil {")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(v)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(raw)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	case KindJSONBMapScalar:
		valGo := goMapScalarValueType(c.MapValueKind)
		g.P("    if src := ", src, "; src != nil {")
		g.P("        dst := make(map[string]", valGo, ", len(src))")
		g.P("        for k, v := range src {")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = v")
		g.P("        }")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(raw)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	case KindJSONBMapEnum:
		g.P("    if src := ", src, "; src != nil {")
		g.P("        dst := make(map[string]string, len(src))")
		g.P("        for k, v := range src {")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = v.String()")
		g.P("        }")
		g.P("        raw, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(raw)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	case KindJSONBMapMessage:
		g.P("    if src := ", src, "; src != nil {")
		g.P("        dst := make(map[string]", qual(g, encodingPkg, "RawMessage"), ", len(src))")
		g.P("        for k, v := range src {")
		g.P("            raw, err := ", qual(g, protojsonPkg, "Marshal"), "(v)")
		g.P("            if err != nil { return m, err }")
		g.P("            dst[", emitKeyToString(g, c.MapKeyKind), "] = raw")
		g.P("        }")
		g.P("        buf, err := ", qual(g, encodingPkg, "Marshal"), "(dst)")
		g.P("        if err != nil { return m, err }")
		g.P("        s := string(buf)")
		g.P("        m.", c.JetFieldName, " = &s")
		g.P("    }")
	}
}

// writeNullableRepeatedPrimitiveAssign emits proto → model for a
// nullable repeated primitive column. Mirrors writeRepeatedPrimitiveModelAssign
// but wraps the pq-typed array value in a pointer so nil proto slice
// ↔ SQL NULL survives the round-trip.
func writeNullableRepeatedPrimitiveAssign(g *protogen.GeneratedFile, c ColumnPlan, src, pqType, castElem string) {
	pqIdent := qual(g, pqPkg, pqType)
	if castElem == "" {
		g.P("    if v := ", src, "; v != nil {")
		g.P("        arr := ", pqIdent, "(v)")
		g.P("        m.", c.JetFieldName, " = &arr")
		g.P("    }")
		return
	}
	g.P("    if v := ", src, "; v != nil {")
	g.P("        arr := make(", pqIdent, ", len(v))")
	g.P("        for i, e := range v { arr[i] = ", castElem, "(e) }")
	g.P("        m.", c.JetFieldName, " = &arr")
	g.P("    }")
}

// writeNullableProtoAssign emits the model → proto branch for a
// nullable column. For proto3-optional scalars / enums the proto field
// is already a pointer (`*string`, `*EnumType`, ...) so we assign the
// dereference directly. For message-kind fields (Timestamp) the proto
// field is a `*Timestamp` message pointer — wrap the dereferenced model
// value via its pb constructor.
// writeWrapperModelAssign emits proto → model for a google.protobuf.*Value
// wrapper field. Presence is the wrapper message pointer's nilness; the
// inner scalar is accessed via .GetValue() (which returns the zero value
// for a nil receiver, but we gate on nilness first to preserve presence).
// Uint wrappers (UInt32Value / UInt64Value) reinterpret via the same
// two's-complement cast as the bare uint proto kinds.
func writeWrapperModelAssign(g *protogen.GeneratedFile, c ColumnPlan, src string) {
	fieldRef := "p." + c.Field.GoName
	g.P("    if ", fieldRef, " != nil {")
	switch c.WrapperCtor {
	case "UInt32":
		g.P("        v := int32(", src, ".GetValue())")
	case "UInt64":
		g.P("        v := int64(", src, ".GetValue())")
	case "Bytes":
		g.P("        v := ", src, ".GetValue()")
	default:
		g.P("        v := ", src, ".GetValue()")
	}
	g.P("        m.", c.JetFieldName, " = &v")
	g.P("    }")
}

// writeWrapperProtoAssign emits model → proto for a wrapper field.
// Dereference the pointer, reinterpret to the wrapper's native Go type
// if needed, and reconstruct the wrapper message via wrapperspb.<Ctor>.
func writeWrapperProtoAssign(g *protogen.GeneratedFile, c ColumnPlan, dst, src string) {
	wrapperCtor := qual(g, wrappersPkg, c.WrapperCtor)
	g.P("    if ", src, " != nil {")
	switch c.WrapperCtor {
	case "UInt32":
		g.P("        ", dst, " = ", wrapperCtor, "(uint32(*", src, "))")
	case "UInt64":
		g.P("        ", dst, " = ", wrapperCtor, "(uint64(*", src, "))")
	default:
		g.P("        ", dst, " = ", wrapperCtor, "(*", src, ")")
	}
	g.P("    }")
}

func writeNullableProtoAssign(g *protogen.GeneratedFile, c ColumnPlan, dst, src string) {
	if c.WrapperCtor != "" {
		writeWrapperProtoAssign(g, c, dst, src)
		return
	}
	switch c.Kind {
	case KindScalar:
		if c.JetGoType == "*[]byte" {
			// Nullable bytes: jet model is *[]byte, proto3-optional
			// bytes is []byte. Dereference when non-nil; otherwise
			// leave the proto field as its zero []byte (nil).
			g.P("    if ", src, " != nil {")
			g.P("        ", dst, " = *", src)
			g.P("    }")
			return
		}
		g.P("    if ", src, " != nil {")
		switch {
		case isUnsignedInt32Kind(c.Field.Desc.Kind()):
			g.P("        v := uint32(*", src, ")")
			g.P("        ", dst, " = &v")
		case isUnsignedInt64Kind(c.Field.Desc.Kind()):
			g.P("        v := uint64(*", src, ")")
			g.P("        ", dst, " = &v")
		default:
			g.P("        v := *", src)
			g.P("        ", dst, " = &v")
		}
		g.P("    }")
	case KindTimestamp:
		g.P("    if ", src, " != nil {")
		g.P("        ", dst, " = ", qual(g, timestampPkg, "New"), "(*", src, ")")
		g.P("    }")
	case KindDuration:
		g.P("    if ", src, " != nil {")
		g.P("        v := ", qual(g, durationPkg, "New"), "(", qual(g, timePkg, "Duration"), "(*", src, "))")
		g.P("        ", dst, " = v")
		g.P("    }")
	case KindEnumAsText:
		enumIdent := g.QualifiedGoIdent(c.Field.Enum.GoIdent)
		g.P("    if ", src, " != nil {")
		g.P("        if v, ok := ", enumIdent, "_value[*", src, "]; ok {")
		g.P("            ev := ", enumIdent, "(v)")
		g.P("            ", dst, " = &ev")
		g.P("        }")
		g.P("    }")
	case KindRepeatedText:
		g.P("    if ", src, " != nil {")
		g.P("        ", dst, " = []string(*", src, ")")
		g.P("    }")
	case KindRepeatedEnum:
		enumIdent := g.QualifiedGoIdent(c.Field.Enum.GoIdent)
		g.P("    if ", src, " != nil {")
		g.P("        names := []string(*", src, ")")
		g.P("        vals := make([]", enumIdent, ", 0, len(names))")
		g.P("        for _, name := range names {")
		g.P("            if v, ok := ", enumIdent, "_value[name]; ok {")
		g.P("                vals = append(vals, ", enumIdent, "(v))")
		g.P("            }")
		g.P("        }")
		g.P("        ", dst, " = vals")
		g.P("    }")
	case KindRepeatedBool:
		g.P("    if ", src, " != nil {")
		g.P("        ", dst, " = []bool(*", src, ")")
		g.P("    }")
	case KindRepeatedInt32:
		if isUnsignedInt32Kind(c.Field.Desc.Kind()) {
			g.P("    if ", src, " != nil {")
			g.P("        items := []int32(*", src, ")")
			g.P("        uitems := make([]uint32, len(items))")
			g.P("        for i, v := range items { uitems[i] = uint32(v) }")
			g.P("        ", dst, " = uitems")
			g.P("    }")
		} else {
			g.P("    if ", src, " != nil {")
			g.P("        ", dst, " = []int32(*", src, ")")
			g.P("    }")
		}
	case KindRepeatedInt64:
		if isUnsignedInt64Kind(c.Field.Desc.Kind()) {
			g.P("    if ", src, " != nil {")
			g.P("        items := []int64(*", src, ")")
			g.P("        uitems := make([]uint64, len(items))")
			g.P("        for i, v := range items { uitems[i] = uint64(v) }")
			g.P("        ", dst, " = uitems")
			g.P("    }")
		} else {
			g.P("    if ", src, " != nil {")
			g.P("        ", dst, " = []int64(*", src, ")")
			g.P("    }")
		}
	case KindRepeatedFloat32:
		g.P("    if ", src, " != nil {")
		g.P("        ", dst, " = []float32(*", src, ")")
		g.P("    }")
	case KindRepeatedFloat64:
		g.P("    if ", src, " != nil {")
		g.P("        ", dst, " = []float64(*", src, ")")
		g.P("    }")
	case KindRepeatedTimestamp:
		g.P("    if ", src, " != nil {")
		g.P("        items := []", qual(g, timePkg, "Time"), "(*", src, ")")
		g.P("        tss := make([]*", qual(g, timestampPkg, "Timestamp"), ", len(items))")
		g.P("        for i, t := range items { tss[i] = ", qual(g, timestampPkg, "New"), "(t) }")
		g.P("        ", dst, " = tss")
		g.P("    }")
	case KindRepeatedDuration:
		g.P("    if ", src, " != nil {")
		g.P("        items := []int64(*", src, ")")
		g.P("        durs := make([]*", qual(g, durationPkg, "Duration"), ", len(items))")
		g.P("        for i, ns := range items { durs[i] = ", qual(g, durationPkg, "New"), "(", qual(g, timePkg, "Duration"), "(ns)) }")
		g.P("        ", dst, " = durs")
		g.P("    }")
	case KindJSONBProto:
		// nil *string -> leave the proto field nil (absent / never set);
		// a present pointer -> unmarshal, so a stored "{}" round-trips to a
		// present-but-empty message distinct from absent. DiscardUnknown drops
		// fields reserved/removed since the row was written.
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.GoIdent)
		g.P("    if ", src, " != nil {")
		g.P("        nested := &", nestedIdent, "{}")
		g.P("        if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal([]byte(*", src, "), nested); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        ", dst, " = nested")
		g.P("    }")
	case KindJSONBProtoList:
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.GoIdent)
		g.P("    if ", src, " != nil {")
		g.P("        var parts []", qual(g, encodingPkg, "RawMessage"))
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(*", src, "), &parts); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        items := make([]*", nestedIdent, ", 0, len(parts))")
		g.P("        for _, raw := range parts {")
		g.P("            item := &", nestedIdent, "{}")
		g.P("            if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal(raw, item); err != nil {")
		g.P("                return nil, err")
		g.P("            }")
		g.P("            items = append(items, item)")
		g.P("        }")
		g.P("        ", dst, " = items")
		g.P("    }")
	case KindJSONBStrMap:
		g.P("    if ", src, " != nil {")
		g.P("        m2 := map[string]string{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(*", src, "), &m2); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        ", dst, " = m2")
		g.P("    }")
	case KindJSONBMapScalar:
		keyGo := goMapKeyType(c.MapKeyKind)
		valGo := goMapScalarValueType(c.MapValueKind)
		g.P("    if ", src, " != nil {")
		g.P("        tmp := map[string]", valGo, "{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(*", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]", valGo, ", len(tmp))")
		g.P("        for ks, v := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            out2[rk] = v")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	case KindJSONBMapEnum:
		enumIdent := g.QualifiedGoIdent(c.Field.Message.Fields[1].Enum.GoIdent)
		keyGo := goMapKeyType(c.MapKeyKind)
		g.P("    if ", src, " != nil {")
		g.P("        tmp := map[string]string{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(*", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]", enumIdent, ", len(tmp))")
		g.P("        for ks, name := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            out2[rk] = ", enumIdent, "(", enumIdent, "_value[name])")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	case KindJSONBMapMessage:
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.Fields[1].Message.GoIdent)
		keyGo := goMapKeyType(c.MapKeyKind)
		g.P("    if ", src, " != nil {")
		g.P("        tmp := map[string]", qual(g, encodingPkg, "RawMessage"), "{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(*", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]*", nestedIdent, ", len(tmp))")
		g.P("        for ks, raw := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            item := &", nestedIdent, "{}")
		g.P("            if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal(raw, item); err != nil {")
		g.P("                return nil, err")
		g.P("            }")
		g.P("            out2[rk] = item")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	}
}

func writeProtoAssignFromModel(g *protogen.GeneratedFile, c ColumnPlan) {
	dst := "out." + c.Field.GoName
	src := "m." + c.JetFieldName
	if c.Nullable {
		writeNullableProtoAssign(g, c, dst, src)
		return
	}
	switch c.Kind {
	case KindScalar:
		// Inverse of the uint-kind conversion in writeModelAssignFromProto:
		// the proto field expects uint*, the jet model surfaces int*.
		switch {
		case c.Field != nil && isUnsignedInt32Kind(c.Field.Desc.Kind()):
			g.P("    ", dst, " = uint32(", src, ")")
		case c.Field != nil && isUnsignedInt64Kind(c.Field.Desc.Kind()):
			g.P("    ", dst, " = uint64(", src, ")")
		default:
			g.P("    ", dst, " = ", src)
		}
	case KindTimestamp:
		g.P("    ", dst, " = ", qual(g, timestampPkg, "New"), "(", src, ")")
	case KindDuration:
		// nanoseconds (BIGINT) -> *durationpb.Duration
		g.P("    ", dst, " = ", qual(g, durationPkg, "New"), "(", qual(g, timePkg, "Duration"), "(", src, "))")
	case KindEnumAsText:
		// enum stored as TEXT: the generated <EnumType>_value[<string>]
		// map gives the int32 for the stored name.
		enumIdent := g.QualifiedGoIdent(c.Field.Enum.GoIdent)
		g.P("    if v, ok := ", enumIdent, "_value[", src, "]; ok {")
		g.P("        ", dst, " = ", enumIdent, "(v)")
		g.P("    }")
	case KindRepeatedText:
		g.P("    ", dst, " = []string(", src, ")")
	case KindRepeatedEnum:
		enumIdent := g.QualifiedGoIdent(c.Field.Enum.GoIdent)
		g.P("    {")
		g.P("        names := []string(", src, ")")
		g.P("        vals := make([]", enumIdent, ", 0, len(names))")
		g.P("        for _, name := range names {")
		g.P("            if v, ok := ", enumIdent, "_value[name]; ok {")
		g.P("                vals = append(vals, ", enumIdent, "(v))")
		g.P("            }")
		g.P("        }")
		g.P("        ", dst, " = vals")
		g.P("    }")
	case KindRepeatedBool:
		g.P("    ", dst, " = []bool(", src, ")")
	case KindRepeatedInt32:
		// Signed int32 kinds copy straight; unsigned kinds reinterpret
		// element-wise back to uint32. Inner `dst`/`src` names differ
		// from the outer `out` proto to avoid shadowing.
		if isUnsignedInt32Kind(c.Field.Desc.Kind()) {
			g.P("    {")
			g.P("        items := []int32(", src, ")")
			g.P("        uitems := make([]uint32, len(items))")
			g.P("        for i, v := range items { uitems[i] = uint32(v) }")
			g.P("        ", dst, " = uitems")
			g.P("    }")
		} else {
			g.P("    ", dst, " = []int32(", src, ")")
		}
	case KindRepeatedInt64:
		if isUnsignedInt64Kind(c.Field.Desc.Kind()) {
			g.P("    {")
			g.P("        items := []int64(", src, ")")
			g.P("        uitems := make([]uint64, len(items))")
			g.P("        for i, v := range items { uitems[i] = uint64(v) }")
			g.P("        ", dst, " = uitems")
			g.P("    }")
		} else {
			g.P("    ", dst, " = []int64(", src, ")")
		}
	case KindRepeatedFloat32:
		g.P("    ", dst, " = []float32(", src, ")")
	case KindRepeatedFloat64:
		g.P("    ", dst, " = []float64(", src, ")")
	case KindRepeatedTimestamp:
		// jettypes.TimestampArray ([]time.Time) -> []*timestamppb.Timestamp.
		// pq.Array returns each element in UTC already, so no extra
		// location normalisation is needed before timestamppb.New.
		g.P("    {")
		g.P("        items := []", qual(g, timePkg, "Time"), "(", src, ")")
		g.P("        tss := make([]*", qual(g, timestampPkg, "Timestamp"), ", len(items))")
		g.P("        for i, t := range items { tss[i] = ", qual(g, timestampPkg, "New"), "(t) }")
		g.P("        ", dst, " = tss")
		g.P("    }")
	case KindRepeatedDuration:
		g.P("    {")
		g.P("        items := []int64(", src, ")")
		g.P("        durs := make([]*", qual(g, durationPkg, "Duration"), ", len(items))")
		g.P("        for i, ns := range items { durs[i] = ", qual(g, durationPkg, "New"), "(", qual(g, timePkg, "Duration"), "(ns)) }")
		g.P("        ", dst, " = durs")
		g.P("    }")
	case KindJSONBProto:
		// JSONB column is typed as string (go-jet default). Cast to
		// []byte for protojson.Unmarshal. DiscardUnknown lets us drop
		// fields that have been removed/reserved in the proto since
		// the row was last written — old persisted state is the rule,
		// not the exception, for resources that have evolved their
		// schema across releases.
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.GoIdent)
		g.P("    if ", src, " != \"\" && ", src, " != \"{}\" {")
		g.P("        nested := &", nestedIdent, "{}")
		g.P("        if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal([]byte(", src, "), nested); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        ", dst, " = nested")
		g.P("    }")
	case KindJSONBProtoList:
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.GoIdent)
		g.P("    if ", src, " != \"\" {")
		g.P("        var parts []", qual(g, encodingPkg, "RawMessage"))
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(", src, "), &parts); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        items := make([]*", nestedIdent, ", 0, len(parts))")
		g.P("        for _, raw := range parts {")
		g.P("            item := &", nestedIdent, "{}")
		g.P("            if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal(raw, item); err != nil {")
		g.P("                return nil, err")
		g.P("            }")
		g.P("            items = append(items, item)")
		g.P("        }")
		g.P("        ", dst, " = items")
		g.P("    }")
	case KindJSONBStrMap:
		g.P("    if ", src, " != \"\" {")
		g.P("        m2 := map[string]string{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(", src, "), &m2); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        ", dst, " = m2")
		g.P("    }")
	case KindJSONBMapScalar:
		// Decode into map[string]V, then re-key into the proto's Go key
		// type. Empty string is treated the same as "{}" (both legal JSON
		// zero states for this column); nil output means a downstream
		// caller asked for an empty map, which in proto3 is
		// indistinguishable from unset — we assign an empty map so the
		// caller gets a non-nil map value as soon as the column decodes
		// to non-zero content.
		keyGo := goMapKeyType(c.MapKeyKind)
		valGo := goMapScalarValueType(c.MapValueKind)
		g.P("    if ", src, " != \"\" {")
		g.P("        tmp := map[string]", valGo, "{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]", valGo, ", len(tmp))")
		g.P("        for ks, v := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            out2[rk] = v")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	case KindJSONBMapEnum:
		// map<K, SomeEnum> — decode as map[string]string (enum names),
		// then re-key into the proto's Go key type and look each name
		// up via <Enum>_value[]. Unknown names fall to the zero value,
		// same convention KindEnumAsText follows.
		enumIdent := g.QualifiedGoIdent(c.Field.Message.Fields[1].Enum.GoIdent)
		keyGo := goMapKeyType(c.MapKeyKind)
		g.P("    if ", src, " != \"\" {")
		g.P("        tmp := map[string]string{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]", enumIdent, ", len(tmp))")
		g.P("        for ks, name := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            out2[rk] = ", enumIdent, "(", enumIdent, "_value[name])")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	case KindJSONBMapMessage:
		// For map<K, VMessage> protogen models the field as a regular
		// message-valued field whose message is the synthetic map-entry.
		// Fields[1] of that entry is the value; its Message is the
		// user's nested type — that's the identifier we need for the
		// generated map[K]*V allocation below.
		nestedIdent := g.QualifiedGoIdent(c.Field.Message.Fields[1].Message.GoIdent)
		keyGo := goMapKeyType(c.MapKeyKind)
		g.P("    if ", src, " != \"\" {")
		g.P("        tmp := map[string]", qual(g, encodingPkg, "RawMessage"), "{}")
		g.P("        if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(", src, "), &tmp); err != nil {")
		g.P("            return nil, err")
		g.P("        }")
		g.P("        out2 := make(map[", keyGo, "]*", nestedIdent, ", len(tmp))")
		g.P("        for ks, raw := range tmp {")
		g.P("            var rk ", keyGo)
		emitStringToKey(g, c.MapKeyKind)
		g.P("            item := &", nestedIdent, "{}")
		g.P("            if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal(raw, item); err != nil {")
		g.P("                return nil, err")
		g.P("            }")
		g.P("            out2[rk] = item")
		g.P("        }")
		g.P("        ", dst, " = out2")
		g.P("    }")
	}
}

func writeOneofProtoAssign(g *protogen.GeneratedFile, oc OneofColumnPlan) {
	g.P("    switch m.", oc.JetKindField, " {")
	for _, vr := range oc.Variants {
		wrap := vr.Field.GoIdent
		g.P("    case \"", vr.VariantName, "\":")
		switch vr.Field.Desc.Kind() {
		case protoreflect.MessageKind:
			variantMsg := vr.Field.Message.GoIdent
			g.P("        nested := &", variantMsg, "{}")
			g.P("        if m.", oc.JetJSONField, " != \"\" {")
			g.P("            if err := (", qual(g, protojsonPkg, "UnmarshalOptions"), "{DiscardUnknown: true}).Unmarshal([]byte(m.", oc.JetJSONField, "), nested); err != nil {")
			g.P("                return nil, err")
			g.P("            }")
			g.P("        }")
			g.P("        out.", oc.Oneof.GoName, " = &", wrap, "{", vr.Field.GoName, ": nested}")
		case protoreflect.EnumKind:
			enumIdent := vr.Field.Enum.GoIdent
			g.P("        var scratch string")
			g.P("        if m.", oc.JetJSONField, " != \"\" {")
			g.P("            if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(m.", oc.JetJSONField, "), &scratch); err != nil {")
			g.P("                return nil, err")
			g.P("            }")
			g.P("        }")
			g.P("        out.", oc.Oneof.GoName, " = &", wrap, "{", vr.Field.GoName, ": ", enumIdent, "(", enumIdent, "_value[scratch])}")
		default:
			goType := protoKindGoType(vr.Field.Desc.Kind())
			g.P("        var scratch ", goType)
			g.P("        if m.", oc.JetJSONField, " != \"\" {")
			g.P("            if err := ", qual(g, encodingPkg, "Unmarshal"), "([]byte(m.", oc.JetJSONField, "), &scratch); err != nil {")
			g.P("                return nil, err")
			g.P("            }")
			g.P("        }")
			g.P("        out.", oc.Oneof.GoName, " = &", wrap, "{", vr.Field.GoName, ": scratch}")
		}
	}
	g.P("    case \"\":")
	if !oc.Optional {
		g.P("        return nil, ", qual(g, fmtPkg, "Errorf"), "(\"", oc.BaseName, " is required\")")
	}
	g.P("    default:")
	g.P("        return nil, ", qual(g, fmtPkg, "Errorf"), "(\"unknown ", oc.BaseName, " variant %q\", m.", oc.JetKindField, ")")
	g.P("    }")
}

// protoKindGoType maps a proto scalar kind to the Go type protoc-gen-go
// uses for it. Message / enum kinds are handled separately at the call
// site because they need the qualified identifier.
func protoKindGoType(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BytesKind:
		return "[]byte"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	}
	return ""
}
