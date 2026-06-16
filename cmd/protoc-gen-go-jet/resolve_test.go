package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	storagev1 "github.com/redpanda-data/protoc-gen-go-jet/gen/go/gojet/v1"
)

func TestSnakeToPascal(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"display_name", "DisplayName"},
		{"tenant_id", "TenantID"},
		{"oauth_providers", "OAuthProviders"},
		{"pkce_required", "PkceRequired"},
		{"token_endpoint_auth_method", "TokenEndpointAuthMethod"},
		{"url", "URL"},
		{"id", "ID"},
		{"llm_providers", "LlmProviders"}, // LLM is not a jet initialism
		{"", ""},
		{"single", "Single"},
		{"api_call", "APICall"},
		{"http_status", "HTTPStatus"},
		{"created_at", "CreatedAt"},
	}
	for _, c := range cases {
		got := snakeToPascal(c.in)
		assert.Equal(t, c.want, got, "snakeToPascal(%q)", c.in)
	}
}

func TestToSnake(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Baseline.
		{"name", "name"},
		{"displayName", "display_name"},
		{"DisplayName", "display_name"},
		{"oauthProvider", "oauth_provider"},
		{"TenantID", "tenant_id"},

		// Consecutive uppercase runs (common in Go APIs for
		// initialisms): `HTTPServer` → `http_server`,
		// `APIKey` → `api_key`, `XMLParser` → `xml_parser`.
		// Insert a separator before the final capital only when
		// followed by a lowercase — otherwise `TenantID` would
		// become `tenant_i_d`.
		{"HTTPServer", "http_server"},
		{"APIKey", "api_key"},
		{"XMLParser", "xml_parser"},
		{"HTTPProxyURL", "http_proxy_url"},
		{"OAuthClient", "o_auth_client"},

		// Numbers stay attached to their adjacent letter rather
		// than getting their own separator — no caller has wanted
		// `int32` → `int_32` and many rely on this shape.
		{"userID2", "user_id2"},
		{"int32Value", "int32_value"},

		// Already-snake_case input stays unchanged (the function
		// is idempotent).
		{"tenant_id", "tenant_id"},
		{"api_key_ref", "api_key_ref"},

		// Empty input returns empty. Safe baseline.
		{"", ""},
	}
	for _, c := range cases {
		got := toSnake(c.in)
		assert.Equal(t, c.want, got, "toSnake(%q)", c.in)
	}
}

func TestParseOrderBy(t *testing.T) {
	parts, err := parseOrderBy("created_at desc, name asc")
	require.NoError(t, err)
	require.Len(t, parts, 2)
	assert.Equal(t, "created_at", parts[0].FieldPath)
	assert.True(t, parts[0].Desc)
	assert.Equal(t, "name", parts[1].FieldPath)
	assert.False(t, parts[1].Desc)

	parts, err = parseOrderBy("display_name")
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.Equal(t, "display_name", parts[0].FieldPath)
	assert.False(t, parts[0].Desc, "no direction defaults to asc")

	_, err = parseOrderBy("")
	assert.Error(t, err, "empty must error")

	_, err = parseOrderBy("name sideways")
	assert.Error(t, err, "unknown direction must error")
}

// TestParseOrderBy_CaseInsensitiveDirection pins that ASC/DESC are
// matched without regard to case, so the proto author can write their
// order clause in whatever style reads best.
func TestParseOrderBy_CaseInsensitiveDirection(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"name DESC", "name desc", "name Desc", "name dEsC"} {
		parts, err := parseOrderBy(in)
		require.NoError(t, err, "input %q", in)
		require.Len(t, parts, 1)
		assert.True(t, parts[0].Desc, "direction parse failed for %q", in)
	}
}

// TestParseOrderBy_SkipsEmptyTerms keeps the parser forgiving about
// stray commas — "a,, b" still produces [a, b] rather than exploding.
func TestParseOrderBy_SkipsEmptyTerms(t *testing.T) {
	t.Parallel()
	parts, err := parseOrderBy("a,, b")
	require.NoError(t, err)
	require.Len(t, parts, 2)
	assert.Equal(t, "a", parts[0].FieldPath)
	assert.Equal(t, "b", parts[1].FieldPath)
}

// TestNormaliseProtoComment pins the protogen-leading-comment →
// single-line SQL COMMENT ON value pipeline. Leading comments arrive
// from protogen as multi-line raw text (one line per source "// "
// comment). The rendered form must strip the leading spaces from
// "// ", drop blank lines, join surviving lines with single spaces,
// and return "" on empty input so the emitter can decide whether to
// emit a COMMENT ON at all.
func TestNormaliseProtoComment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "blank lines only are empty", in: "   \n\t\n", want: ""},
		{name: "single line strips leading space", in: " the name", want: "the name"},
		{
			name: "two lines join with single space",
			in:   " first line\n second line",
			want: "first line second line",
		},
		{
			name: "blank interior lines dropped",
			in:   " first\n\n second",
			want: "first second",
		},
		{
			name: "trailing blank lines dropped",
			in:   " one\n two\n\n",
			want: "one two",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normaliseProtoComment(tt.in))
		})
	}
}

func TestGoJetStructName(t *testing.T) {
	// Parallels go-jet's own introspection. "oauth_providers" is the
	// acid test: without the exceptions table we'd get "OauthProviders"
	// and the plugin's mapper would fail to compile against the live
	// jet-generated model.
	assert.Equal(t, "LlmProviders", goJetStructName("llm_providers"))
	assert.Equal(t, "McpServers", goJetStructName("mcp_servers"))
	assert.Equal(t, "OAuthProviders", goJetStructName("oauth_providers"))
}

// extractProtoFieldNames reads `options.proto` and returns the
// field names declared in the named message block. Stops at the
// first `\n}` after the opening `message NAME {` line. Skips
// `reserved` clauses and comment-only lines.
func extractProtoFieldNames(t *testing.T, body, messageName string) []string {
	t.Helper()
	marker := "message " + messageName + " {"
	start := strings.Index(body, marker)
	require.NotEqual(t, -1, start, "marker %q not found", marker)
	rest := body[start:]
	end := strings.Index(rest, "\n}")
	require.NotEqual(t, -1, end)
	block := rest[:end]
	var names []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") || line == "" || strings.HasPrefix(line, "reserved") || strings.HasPrefix(line, "message ") {
			continue
		}
		tokens := strings.Fields(line)
		// Shape A: `<type> <name> = <N>;`
		if len(tokens) >= 3 && tokens[2] == "=" {
			names = append(names, tokens[1])
			continue
		}
		// Shape B: `repeated <type> <name> = <N>;`
		if len(tokens) >= 4 && tokens[0] == "repeated" && tokens[3] == "=" {
			names = append(names, tokens[2])
		}
	}
	return names
}

// TestColumnOptionsDocumented asserts every field on the
// `gojet.v1.Column` message appears by name in both the
// user-facing README and the capability CHANGELOG. Catches the
// "add a new Column option, ship the plugin change, forget to
// update the docs" drift that risk #5 in TODO.md names.
//
// Matcher is deliberately loose — a field name anywhere in the
// doc counts. Robust against arbitrary prose edits; still fires
// when a new option lands without any mention.
func TestColumnOptionsDocumented(t *testing.T) {
	t.Parallel()

	optionsBody, err := os.ReadFile(filepath.Join("..", "..", "proto", "gojet", "v1", "options.proto"))
	require.NoError(t, err)

	fieldNames := extractProtoFieldNames(t, string(optionsBody), "Column")
	require.NotEmpty(t, fieldNames, "failed to extract any Column fields from options.proto")

	// Read the README + CHANGELOG.
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	changelog, err := os.ReadFile("CHANGELOG.md")
	require.NoError(t, err)

	// Strict match: look for the field name inside ANY backtick-
	// fenced reference. Table rows use `\`name\``; prose cites
	// the option as `\`skip: true\`` or `\`gojet.v1.column.skip\``.
	// Accepting any backtick wrapper filters out prose mentions
	// like "skip the step" while still matching the doc shapes
	// that actually reference the option.
	hasBacktickedRef := func(doc, name string) bool {
		// Walk every backtick-delimited span and check if it
		// contains the name as a word boundary (preceded by either
		// nothing, whitespace, or a character that can't be part
		// of a proto identifier — i.e. not `a-zA-Z0-9_`).
		spans := strings.Split(doc, "`")
		// Every odd-indexed entry is inside backticks.
		for i := 1; i < len(spans); i += 2 {
			s := spans[i]
			idx := strings.Index(s, name)
			if idx == -1 {
				continue
			}
			// Check boundary before.
			if idx > 0 {
				prev := s[idx-1]
				if isIdentChar(prev) {
					continue
				}
			}
			// Check boundary after.
			after := idx + len(name)
			if after < len(s) {
				next := s[after]
				if isIdentChar(next) {
					continue
				}
			}
			return true
		}
		return false
	}

	for _, name := range fieldNames {
		assert.True(t, hasBacktickedRef(string(readme), name),
			"Column option `%s` isn't referenced in README.md (inside backticks) — add a row to the options reference table", name)
		// CHANGELOG uses `gojet.v1.column.<name>` as the leading
		// bullet shape. Require that specific pattern so a REMOVED
		// entry trips the test — hasBacktickedRef would tolerate
		// any `{ name: ... }` usage mention and miss the real drift.
		canonical := "`gojet.v1.column." + name + "`"
		assert.Contains(t, string(changelog), canonical,
			"Column option `%s` isn't listed as `gojet.v1.column.%s` in CHANGELOG.md — add a bullet to the capability list", name, name)
	}
}

// isIdentChar reports whether c is legal inside a proto3 field
// identifier. Used by the doc-sync test's word-boundary check so
// `skip` doesn't match inside `skipped` / `skipper` / etc.
func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

// TestTableOptionsDocumented is the Table-option sibling of
// TestColumnOptionsDocumented. Catches the same drift class on
// the message-level annotation surface.
//
// `output` is the one exception — the README explicitly deprecates
// it (derived from proto package layout now); it's present in
// the proto as an escape hatch but not meant to be documented
// in the primary reference.
func TestTableOptionsDocumented(t *testing.T) {
	t.Parallel()
	optionsBody, err := os.ReadFile(filepath.Join("..", "..", "proto", "gojet", "v1", "options.proto"))
	require.NoError(t, err)
	fieldNames := extractProtoFieldNames(t, string(optionsBody), "Table")
	require.NotEmpty(t, fieldNames)

	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)

	for _, name := range fieldNames {
		if name == "output" {
			continue // deprecated-in-place escape hatch
		}
		// README's Table-option reference table uses `| \`name\` |`.
		assert.Contains(t, string(readme), "`"+name+"`",
			"Table option `%s` isn't referenced in README.md — add a row to the Table reference table", name)
	}
}

// TestOneofColumnOptionsDocumented is the OneofColumn-option
// sibling. Smaller option surface (three fields) but the same
// drift risk.
func TestOneofColumnOptionsDocumented(t *testing.T) {
	t.Parallel()
	optionsBody, err := os.ReadFile(filepath.Join("..", "..", "proto", "gojet", "v1", "options.proto"))
	require.NoError(t, err)
	fieldNames := extractProtoFieldNames(t, string(optionsBody), "OneofColumn")
	require.NotEmpty(t, fieldNames)

	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)

	for _, name := range fieldNames {
		assert.Contains(t, string(readme), "`"+name+"`",
			"OneofColumn option `%s` isn't referenced in README.md — add a row to the OneofColumn reference table", name)
	}
}

// TestJetSnakerMirror_InSyncWithUpstream reads go-jet's snaker.go
// from the module cache at the version pinned in go.mod and
// asserts our `jetAcronyms` + `jetSnakeExceptions` maps are byte-
// exact with jet's `commonInitialisms` + `snakeToCamelExceptions`.
//
// This catches the "go-jet upgrade silently added a new
// initialism" drift before the next regen surfaces it as a
// mapper compile error. The snaker is in `internal/3rdparty` so
// we can't import it directly; read the source file instead.
//
// Skips (rather than fails) if the module cache isn't available
// — e.g. a CI pipeline running without module downloads. Local
// `go test` runs always have it.
func TestJetSnakerMirror_InSyncWithUpstream(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skipf("go env GOMODCACHE unavailable: %v", err)
	}
	modCache := strings.TrimSpace(string(out))
	if modCache == "" {
		t.Skip("empty GOMODCACHE")
	}

	// go-jet version from go.mod. Parse instead of hardcoding so
	// the test keeps working across upgrades.
	goModBytes, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	var jetVersion string
	for _, line := range strings.Split(string(goModBytes), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "github.com/go-jet/jet/v2 ") {
			jetVersion = strings.TrimSpace(strings.TrimPrefix(line, "github.com/go-jet/jet/v2"))
			break
		}
	}
	require.NotEmpty(t, jetVersion, "could not find go-jet version in go.mod")

	snakerPath := filepath.Join(modCache, "github.com", "go-jet", "jet", "v2@"+jetVersion, "internal", "3rdparty", "snaker", "snaker.go")
	body, err := os.ReadFile(snakerPath) //nolint:gosec // test-only file read from go module cache
	if err != nil {
		t.Skipf("snaker source unavailable at %s: %v", snakerPath, err)
	}

	// Extract entries between `commonInitialisms = map[string]bool{`
	// and the closing `}`. Same for snakeToCamelExceptions.
	extractStringKeys := func(src, marker string) map[string]bool {
		start := strings.Index(src, marker)
		require.NotEqual(t, -1, start, "marker %q not found in snaker.go", marker)
		rest := src[start:]
		end := strings.Index(rest, "\n}")
		require.NotEqual(t, -1, end)
		block := rest[:end]
		out := map[string]bool{}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			start := strings.Index(line, `"`)
			if start == -1 {
				continue
			}
			end := strings.Index(line[start+1:], `"`)
			if end == -1 {
				continue
			}
			out[line[start+1:start+1+end]] = true
		}
		return out
	}

	upstreamAcronyms := extractStringKeys(string(body), "commonInitialisms = map[string]bool{")
	// jet's commonInitialisms lookup uppercases the word before
	// the map check (snaker.go:73: `if upper := strings.ToUpper(word);
	// commonInitialisms[upper] { ... }`). Any mixed-case key in that
	// map is a dead entry — jet never looks it up. Filter to the
	// all-uppercase entries for the comparison.
	filterUpper := func(m map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k := range m {
			if k == strings.ToUpper(k) {
				out[k] = true
			}
		}
		return out
	}
	upstreamAcronyms = filterUpper(upstreamAcronyms)
	for k := range jetAcronyms {
		assert.True(t, upstreamAcronyms[k], "our jetAcronyms has %q but go-jet %s doesn't — remove from our mirror", k, jetVersion)
	}
	for k := range upstreamAcronyms {
		assert.True(t, jetAcronyms[k], "go-jet %s has %q in commonInitialisms but our jetAcronyms doesn't — add to resolve.go", jetVersion, k)
	}

	upstreamExceptions := extractStringKeys(string(body), "snakeToCamelExceptions = map[string]string{")
	for k := range jetSnakeExceptions {
		assert.True(t, upstreamExceptions[k], "our jetSnakeExceptions has %q but go-jet %s doesn't", k, jetVersion)
	}
	for k := range upstreamExceptions {
		_, have := jetSnakeExceptions[k]
		assert.True(t, have, "go-jet %s has exception %q but our jetSnakeExceptions doesn't — add to resolve.go", jetVersion, k)
	}
}

// TestImpliesImmutable_NilSafe pins the defensive nil / empty-field
// paths. The positive cases (IDENTIFIER / IMMUTABLE trigger true,
// OUTPUT_ONLY / REQUIRED / etc. don't) ride through the e2e suite
// as real-world protos that carry these annotations.
func TestImpliesImmutable_NilSafe(t *testing.T) {
	t.Parallel()
	// Nil field — can happen only if a caller mis-uses the helper,
	// but the defensive early return keeps the plugin from panicking.
	assert.False(t, impliesImmutable(nil))
}

// TestOtherSkipViolation pins the skip-with-other-option detector.
// A field annotated `skip: true` + any other meaningful column
// option is contradictory: skip says "don't store", the other
// option describes HOW to store. The detector returns the name of
// the first conflicting field so the error message can quote it.
func TestOtherSkipViolation(t *testing.T) {
	t.Parallel()
	// Only skip set → no violation.
	assert.Equal(t, "", otherSkipViolation(&storagev1.Column{Skip: true}))

	// Each other option, one at a time.
	tests := []struct {
		col  *storagev1.Column
		want string
	}{
		{&storagev1.Column{Skip: true, Immutable: true}, "immutable: true"},
		{&storagev1.Column{Skip: true, Orderable: true}, "orderable: true"},
		{&storagev1.Column{Skip: true, Unique: true}, "unique: true"},
		{&storagev1.Column{Skip: true, Nullable: true}, "nullable: true"},
		{&storagev1.Column{Skip: true, AllowZeroEnum: true}, "allow_zero_enum: true"},
		{&storagev1.Column{Skip: true, JsonbGinIndex: true}, "jsonb_gin_index: true"},
		{&storagev1.Column{Skip: true, Name: "foo"}, "name"},
		{&storagev1.Column{Skip: true, DefaultExpr: "''"}, "default_expr"},
		{&storagev1.Column{Skip: true, Check: "x > 0"}, "check"},
		{&storagev1.Column{Skip: true, RenameFrom: "old"}, "rename_from"},
		{&storagev1.Column{Skip: true, Storage: storagev1.Storage_STORAGE_JSONB_PROTO}, "storage"},
		{&storagev1.Column{Skip: true, JsonbIndexedPaths: []string{"a"}}, "jsonb_indexed_paths"},
		{&storagev1.Column{Skip: true, GeneratedExpr: "1 + 1"}, "generated_expr"},
	}
	for _, tt := range tests {
		got := otherSkipViolation(tt.col)
		assert.Equal(t, tt.want, got, "col=%+v", tt.col)
	}

	// Empty Column (default proto3 values) → no violation. The
	// check is skip-specific; a bare `{}` column shouldn't trip.
	assert.Equal(t, "", otherSkipViolation(&storagev1.Column{}))
}

// TestApplyGeneratedExpr_HappyPath pins the default success path: a
// non-empty expression with no conflicting options stores the
// expression on the plan and returns nil.
func TestApplyGeneratedExpr_HappyPath(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{GeneratedExpr: "a + b"}
	c := &ColumnPlan{}
	require.NoError(t, applyGeneratedExpr(col, c, "total"))
	assert.Equal(t, "a + b", c.GeneratedExpr)
}

// TestApplyGeneratedExpr_EmptyIsNoop confirms a missing expression is
// a no-op — the plan's GeneratedExpr stays empty and no validation
// fires.
func TestApplyGeneratedExpr_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{}
	c := &ColumnPlan{}
	require.NoError(t, applyGeneratedExpr(col, c, "total"))
	assert.Empty(t, c.GeneratedExpr)
}

// TestApplyGeneratedExpr_SemicolonRejected pins the same `;`-as-
// statement-terminator guard the resolver applies to default_expr and
// check. Without the guard, a generation expression containing `;`
// would close the CREATE TABLE prematurely and let a trailing
// statement run unintended.
func TestApplyGeneratedExpr_SemicolonRejected(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{GeneratedExpr: "a + b; DROP TABLE x"}
	c := &ColumnPlan{}
	err := applyGeneratedExpr(col, c, "total")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain ';'")
	assert.Contains(t, err.Error(), `"total"`,
		"error must name the offending proto field")
}

// TestApplyGeneratedExpr_DefaultExprConflict pins the PG-enforced
// rule: a generated column cannot also carry DEFAULT. Catching this
// at resolve time produces a clear message instead of a CREATE TABLE
// syntax error at apply time.
//
// The conflict is keyed off the USER-supplied annotation
// (`col.GetDefaultExpr()`) rather than the resolved `c.DefaultExpr`,
// because `inferFieldEncoding` auto-fills kind defaults (e.g. "0"
// for int64) before this validator runs. The auto-default is not a
// real conflict — the DDL emitter's GENERATED branch suppresses it.
func TestApplyGeneratedExpr_DefaultExprConflict(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{GeneratedExpr: "a + b", DefaultExpr: "0"}
	c := &ColumnPlan{DefaultExpr: "0"}
	err := applyGeneratedExpr(col, c, "total")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_expr")
	assert.Contains(t, err.Error(), "Postgres rejects DEFAULT alongside GENERATED")
}

// TestApplyGeneratedExpr_AutoInferredDefaultIgnored pins the
// counterpart: the resolved `c.DefaultExpr` may carry a kind-
// inferred default (e.g. "0" for int64) at the time the validator
// runs, and that must NOT trip the conflict guard. The applier
// also clears `c.DefaultExpr` so downstream consumers see a
// coherent generated-column state.
func TestApplyGeneratedExpr_AutoInferredDefaultIgnored(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{GeneratedExpr: "a + b"} // no user-set default
	c := &ColumnPlan{DefaultExpr: "0"}               // kind-inferred
	require.NoError(t, applyGeneratedExpr(col, c, "total"))
	assert.Equal(t, "a + b", c.GeneratedExpr)
	assert.Empty(t, c.DefaultExpr, "auto-inferred default must be cleared on a generated column")
}

// TestApplyGeneratedExpr_UniqueConflict pins the out-of-scope decision:
// a unique constraint must be declared on a regular column, not a
// generated one, so the index is unambiguously associated with the
// underlying source columns.
func TestApplyGeneratedExpr_UniqueConflict(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{GeneratedExpr: "a + b", Unique: true}
	c := &ColumnPlan{}
	err := applyGeneratedExpr(col, c, "total")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unique")
	assert.Contains(t, err.Error(), "regular column")
}

// TestApplyGeneratedExpr_ForeignKeyConflict pins the same out-of-scope
// decision for foreign keys.
func TestApplyGeneratedExpr_ForeignKeyConflict(t *testing.T) {
	t.Parallel()
	col := &storagev1.Column{
		GeneratedExpr: "a + b",
		ForeignKey:    &storagev1.ForeignKey{Target: "Other.id"},
	}
	c := &ColumnPlan{}
	err := applyGeneratedExpr(col, c, "total")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreign_key")
	assert.Contains(t, err.Error(), "regular column")
}

// TestJetFieldNameFromDB pins the DB-column → model-field mapping.
// Kept as its own test (instead of subsumed by TestSnakeToPascal)
// because the conceptual contract differs: goJetStructName
// pascalises a TABLE name, jetFieldNameFromDB pascalises a COLUMN
// name. A future refactor could diverge the two — the test
// confirms today's equivalence is deliberate.
func TestJetFieldNameFromDB(t *testing.T) {
	t.Parallel()
	// Every column name the e2e protos produce.
	cases := map[string]string{
		"id":                   "ID", // acronym exception
		"tenant_id":            "TenantID",
		"user_id":              "UserID",
		"name":                 "Name",
		"display_name":         "DisplayName",
		"created_at":           "CreatedAt",
		"updated_at":           "UpdatedAt",
		"api_url":              "APIURL", // two consecutive acronyms
		"provider_config_kind": "ProviderConfigKind",
		"provider_config":      "ProviderConfig",
		"oauth_client_id":      "OAuthClientID", // exception word + acronym
		"jsonb_data":           "JsonbData",     // not an acronym — plain title-case
	}
	for db, want := range cases {
		assert.Equal(t, want, jetFieldNameFromDB(db), "jetFieldNameFromDB(%q)", db)
	}
}

// resolveIndex translates a (gojet.v1.Index) proto option into the
// internal IndexPlan. Four things the test matrix pins: default name
// generation, per-column order defaulting to ASC, explicit ASC/DESC
// accepted case-insensitively, and empty / invalid inputs rejected.
//
// Tests go through the proto type directly — no protogen descriptors
// needed, the function is pure.
func TestResolveIndex_DefaultName(t *testing.T) {
	t.Parallel()
	got, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"tenant_id", "name"},
	})
	require.NoError(t, err)
	assert.Equal(t, "idx_things_tenant_id_name", got.Name, "auto-name from table+columns")
	assert.Equal(t, []string{"tenant_id", "name"}, got.Columns)
	assert.Equal(t, []string{"ASC", "ASC"}, got.Order, "missing order defaults to ASC per column")
	assert.False(t, got.Unique)
	assert.Empty(t, got.Where)
}

func TestResolveIndex_ExplicitNameAndOrder(t *testing.T) {
	t.Parallel()
	got, err := resolveIndex("things", &storagev1.Index{
		Name:    "idx_custom",
		Columns: []string{"created_at", "name"},
		Order:   []string{"DESC", "ASC"},
		Unique:  true,
		Where:   "deleted_at IS NULL",
	})
	require.NoError(t, err)
	assert.Equal(t, "idx_custom", got.Name)
	assert.Equal(t, []string{"DESC", "ASC"}, got.Order)
	assert.True(t, got.Unique)
	assert.Equal(t, "deleted_at IS NULL", got.Where)
}

// TestResolveIndex_WhereRejectsSemicolon pins that the partial-index
// `where` predicate is expression-shaped — a `;` would end the
// enclosing CREATE INDEX prematurely and the trailing SQL would
// execute as a separate statement. Belt-and-braces against
// injection by a proto author who pastes a full statement.
func TestResolveIndex_WhereRejectsSemicolon(t *testing.T) {
	t.Parallel()
	_, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"id"},
		Where:   "enabled = true; DROP TABLE users",
	})
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "must not contain")
	}
}

// TestResolveIndex_NameIdentValidated pins that the override name
// flows through validateSQLIdent. Same SQL injection / case-fold /
// leading-digit footguns apply as for Table.name.
func TestResolveIndex_NameIdentValidated(t *testing.T) {
	t.Parallel()
	bad := []string{"MyIndex", "idx with space", "idx-1", "drop; --"}
	for _, name := range bad {
		_, err := resolveIndex("things", &storagev1.Index{
			Name:    name,
			Columns: []string{"id"},
		})
		if assert.Errorf(t, err, "index name %q should be rejected", name) {
			assert.Contains(t, err.Error(), "snake_case")
		}
	}
}

func TestResolveIndex_OrderIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	got, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"a", "b"},
		Order:   []string{"asc", "desc"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"ASC", "DESC"}, got.Order, "stored form must be upper-case")
}

func TestResolveIndex_ShorterOrderSliceDefaultsToASC(t *testing.T) {
	t.Parallel()
	// A proto author may only specify order for the first column and
	// expect the rest to default — that's the intent of parallel-slice
	// encoding with an implicit ASC fallback.
	got, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"a", "b", "c"},
		Order:   []string{"DESC"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"DESC", "ASC", "ASC"}, got.Order)
}

func TestResolveIndex_NoColumnsErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveIndex("things", &storagev1.Index{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one column")
}

func TestResolveIndex_InvalidOrderErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"a"},
		Order:   []string{"sideways"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ASC or DESC")
}

// TestResolveIndex_TooManyOrderEntriesErrors catches a silent data loss
// bug — more Order entries than Columns would be dropped on the floor
// and leave the author thinking their override was honored.
func TestResolveIndex_TooManyOrderEntriesErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"a", "b"},
		Order:   []string{"ASC", "DESC", "ASC"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "silently dropped")
}

// TestResolveIndex_DuplicateColumnErrors pins rejection of a column
// name that appears twice in the same index. PG rejects these at
// CREATE INDEX — catching at generate saves a trip through migration
// generation + apply to hit the same error later.
func TestResolveIndex_DuplicateColumnErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveIndex("things", &storagev1.Index{
		Columns: []string{"tenant_id", "name", "tenant_id"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than once")
}

func TestValOr(t *testing.T) {
	assert.Equal(t, "a", valOr("a", "b"))
	assert.Equal(t, "b", valOr("", "b"))
	assert.Equal(t, "", valOr("", ""))
}

// TestValidateSQLIdent pins the snake_case-only identifier rule for
// Table.name and Column.name options. The guard catches three
// distinct footguns: SQL injection via embedded metacharacters,
// silent case-folding when PG downcases unquoted identifiers (the
// plugin emits unquoted, so MyTable silently becomes mytable at
// apply time), and leading-digit names that'd fail at PG parse
// with a cryptic error.
func TestValidateSQLIdent(t *testing.T) {
	t.Parallel()
	ok := []string{
		"name",
		"display_name",
		"tenant_id",
		"x",
		"_private",
		"col1",
		"a_b_c_d",
		"api_key_ref",
		// Exactly 63 bytes — at the PG NAMEDATALEN-1 boundary, must pass.
		"a" + strings.Repeat("b", 62),
	}
	for _, s := range ok {
		assert.NoErrorf(t, validateSQLIdent(s), "identifier %q must be accepted", s)
	}
	bad := map[string]string{
		"":          "must not be empty",
		"Name":      "snake_case",
		"MyTable":   "snake_case",
		"1table":    "snake_case",
		"my-name":   "snake_case",
		"my name":   "snake_case",
		"my.table":  "snake_case",
		`"quoted"`:  "snake_case",
		"drop; --":  "snake_case",
		"日本語":       "snake_case",
		"trailing ": "snake_case",
		" leading":  "snake_case",
		// 64 bytes — one over the NAMEDATALEN-1 limit.
		"a" + strings.Repeat("b", 63): "exceeds Postgres NAMEDATALEN",
	}
	for s, want := range bad {
		err := validateSQLIdent(s)
		if assert.Errorf(t, err, "identifier %q must be rejected", s) {
			assert.Contains(t, err.Error(), want)
		}
	}
}

func TestValidateJSONBPath(t *testing.T) {
	ok := []string{
		"a", "a.b", "api_key_ref", "nested.path.to.leaf",
		// camelCase accepted — PG folds the emitted index name to
		// lowercase, jsonbPathIndexName lowercases to match, so
		// round-trips are stable.
		"installPackVersion",
		// Digits inside / trailing segments.
		"v2", "field_1", "a.b2.c_3",
	}
	for _, p := range ok {
		assert.NoError(t, validateJSONBPath(p), "path %q should be valid", p)
	}
	bad := map[string]string{
		"":       "must not be empty",
		".a":     "stray dot",
		"a.":     "stray dot",
		"a..b":   "stray dot",
		"a.'b":   "quote",
		"a.\"b":  "quote",
		"a\\b":   "backslash",
		"a\nb":   "control",
		"a\tb":   "control",
		"a\x00b": "control",
		"a\x7fb": "control",
		// Hyphens in JSON keys are legit (e.g. `x-request-id`) but
		// would land in an unquoted PG index name and fail to parse
		// at apply — force the caller through custom_sql.
		"with-dash":     "[a-zA-Z0-9_]",
		"x-request-id":  "[a-zA-Z0-9_]",
		"a.with-dash.b": "[a-zA-Z0-9_]",
		// Non-ASCII letters would produce a UTF-8 index name. PG
		// technically accepts it, but it's not snake_case and
		// NAMEDATALEN counts bytes — byte-count overruns trigger
		// silent truncation.
		"日本語":     "[a-zA-Z0-9_]",
		"a.日本語.b": "[a-zA-Z0-9_]",
		// Leading digit on a segment — unquoted identifiers can't
		// start with a digit, but inside a synthesised name like
		// `idx_t_c_2b` the segment position always follows an
		// underscore, so this case is actually safe. Stays valid.
	}
	for p, want := range bad {
		err := validateJSONBPath(p)
		if assert.Errorf(t, err, "path %q should be rejected", p) {
			assert.Contains(t, err.Error(), want)
		}
	}
}

func TestIsJSONBKind(t *testing.T) {
	for _, k := range []ColumnKind{
		KindJSONBProto, KindJSONBProtoList, KindJSONBStrMap,
		KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage,
	} {
		assert.True(t, isJSONBKind(k), "kind %d must be JSONB-backed", k)
	}
	for _, k := range []ColumnKind{
		KindScalar, KindTimestamp, KindDuration, KindEnumAsText,
		KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindSynthesized,
	} {
		assert.False(t, isJSONBKind(k), "kind %d must not be JSONB-backed", k)
	}
}

// TestColumnKindString pins that every ColumnKind value has a
// human-readable name — error messages using %s on a ColumnKind
// should read "got repeated <string>" rather than "got 6". Also
// the fallback catches any future iota addition that skips
// updating the Stringer.
// TestDefaultsToNullable pins the allow-list of column kinds that
// auto-promote to nullable. Adding a new ColumnKind forces an explicit
// opt-in here — silently defaulting to NOT NULL (and discarding the
// nil-vs-empty distinction) is the bug we're guarding against.
func TestDefaultsToNullable(t *testing.T) {
	t.Parallel()
	nullable := []ColumnKind{
		KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindJSONBProtoList,
		KindJSONBStrMap, KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage,
	}
	for _, k := range nullable {
		assert.Truef(t, defaultsToNullable(k), "kind %s must default to nullable", k)
	}
	notNullable := []ColumnKind{
		KindScalar, KindTimestamp, KindDuration, KindEnumAsText,
		KindJSONBProto, KindSynthesized,
	}
	for _, k := range notNullable {
		assert.Falsef(t, defaultsToNullable(k), "kind %s must not default to nullable", k)
	}
	// Exhaustiveness check — the two lists together must cover every
	// defined ColumnKind so a new iota can't sneak past unclassified.
	assert.Equal(t, 14+6, len(nullable)+len(notNullable),
		"nullable + notNullable must cover every ColumnKind; if you added a kind, classify it here")
}

// TestApplyNullable_CollectionKinds verifies applyNullable accepts each
// of the 14 collection kinds, promotes JetGoType to pointer form, and
// clears DefaultExpr when the caller didn't set one. These are the
// kinds that resolveColumn auto-marks Nullable = true.
func TestApplyNullable_CollectionKinds(t *testing.T) {
	t.Parallel()
	kinds := []ColumnKind{
		KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindJSONBProtoList,
		KindJSONBStrMap, KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage,
	}
	for _, k := range kinds {
		c := &ColumnPlan{
			Kind:        k,
			JetGoType:   "[]string",
			DefaultExpr: "'{}'",
		}
		require.NoErrorf(t, applyNullable(c, ""), "kind %s must accept nullable", k)
		assert.Equalf(t, "*[]string", c.JetGoType, "kind %s JetGoType must be pointer-promoted", k)
		assert.Emptyf(t, c.DefaultExpr, "kind %s DefaultExpr must be cleared when userDefault is empty", k)
	}
}

// TestApplyNullable_PreservesUserDefault — if the caller set an
// explicit default_expr, applyNullable must not clobber it. This is
// the opt-out path: user wants nullable storage but with a seeded
// default value on INSERT.
func TestApplyNullable_PreservesUserDefault(t *testing.T) {
	t.Parallel()
	c := &ColumnPlan{
		Kind:        KindRepeatedText,
		JetGoType:   "[]string",
		DefaultExpr: "ARRAY[]::text[]",
	}
	require.NoError(t, applyNullable(c, "ARRAY[]::text[]"))
	assert.Equal(t, "*[]string", c.JetGoType)
	assert.Equal(t, "ARRAY[]::text[]", c.DefaultExpr,
		"applyNullable must preserve a caller-supplied default_expr")
}

// TestApplyNullable_RejectsUnsupportedKinds — applyNullable is
// allow-listed, so KindSynthesized (tenant_id etc., storage-owned with no
// proto presence) must be refused with a clear message.
func TestApplyNullable_RejectsUnsupportedKinds(t *testing.T) {
	t.Parallel()
	for _, k := range []ColumnKind{KindSynthesized} {
		c := &ColumnPlan{
			Kind:      k,
			JetGoType: "string",
		}
		err := applyNullable(c, "")
		require.Errorf(t, err, "kind %s must reject applyNullable", k)
		assert.Containsf(t, err.Error(), "nullable is not supported on this kind",
			"kind %s error message must explain the rejection", k)
	}
}

// TestApplyNullable_JSONBProto — a single nested message (JSONB) opts into
// nullable explicitly (it does NOT default to nullable, unlike collections).
// When it does, applyNullable accepts it (presence via the message pointer, no
// proto3 `optional` needed), pointer-promotes the model type, and clears the
// auto "{}" default so a nil message persists as SQL NULL rather than "{}".
func TestApplyNullable_JSONBProto(t *testing.T) {
	t.Parallel()
	c := &ColumnPlan{
		Kind:        KindJSONBProto,
		JetGoType:   "string",
		DefaultExpr: "'{}'",
	}
	require.NoError(t, applyNullable(c, ""), "nullable JSONB proto must be accepted")
	assert.Equal(t, "*string", c.JetGoType, "model type must be pointer-promoted for NULL")
	assert.Empty(t, c.DefaultExpr, "the auto '{}' default must be cleared so nil -> NULL, not '{}'")
}

func TestColumnKindString(t *testing.T) {
	t.Parallel()
	kinds := []ColumnKind{
		KindScalar, KindTimestamp, KindDuration, KindEnumAsText,
		KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindJSONBProto, KindJSONBProtoList, KindJSONBStrMap,
		KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage,
		KindSynthesized,
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		s := k.String()
		assert.NotContains(t, s, "ColumnKind(", "kind %d must have an explicit name", k)
		assert.False(t, seen[s], "duplicate name %q for kind %d", s, k)
		seen[s] = true
	}
	// Fallback branch pins the shape — used if a caller adds a
	// new iota value and forgets to extend the switch.
	unknown := ColumnKind(9999)
	assert.Equal(t, "ColumnKind(9999)", unknown.String())
}

// TestInferFieldEncoding_RepeatedBytesRejected pins that `repeated bytes`
// fields are rejected at resolve time with the documented error
// message. pq ships no BYTEA[] scanner; if someone relaxes the check,
// the generated code won't compile and the failure mode is a
// confusing Go-compile error rather than a clear "not supported"
// message. This test is the direct regression guard.
func TestInferFieldEncoding_RepeatedBytesRejected(t *testing.T) {
	t.Parallel()

	// Build a synthetic file descriptor with one message containing a
	// single `repeated bytes` field. protodesc gives us a real
	// protoreflect.FieldDescriptor; protogen.Field wraps it with no
	// additional setup since inferFieldEncoding only touches Desc.
	label := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	kind := descriptorpb.FieldDescriptorProto_TYPE_BYTES
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("synthetic.proto"),
		Package: proto.String("synthetic"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Probe"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("data"),
				Number: proto.Int32(1),
				Label:  &label,
				Type:   &kind,
			}},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	require.NoError(t, err)
	require.Equal(t, 1, fd.Messages().Len())
	msg := fd.Messages().Get(0)
	require.Equal(t, 1, msg.Fields().Len())
	field := msg.Fields().Get(0)
	require.True(t, field.IsList())

	pfield := &protogen.Field{Desc: field}
	col := &storagev1.Column{}
	c := &ColumnPlan{}
	err = inferFieldEncoding(pfield, col, c)
	require.Error(t, err, "repeated bytes must be rejected")
	assert.Contains(t, err.Error(), "repeated bytes not supported",
		"error must name the rejected construct")
	assert.Contains(t, err.Error(), "BYTEA[]",
		"error must reference the missing BYTEA[] scanner so a reader knows WHY")
}

// TestInferMapEncoding_FloatDoubleValueRejected pins that map values
// with float / double scalar kinds fail at resolve time. The plugin
// doesn't landed KindJSONBMapScalar support for those widths yet —
// if someone relaxes this without landing the value-codec branch,
// the generated mapper won't compile. Regression guard.
func TestInferMapEncoding_FloatDoubleValueRejected(t *testing.T) {
	t.Parallel()

	for _, kind := range []descriptorpb.FieldDescriptorProto_Type{
		descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		descriptorpb.FieldDescriptorProto_TYPE_DOUBLE,
	} {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			// A map<string, float> or map<string, double> in proto3 is
			// structured as a synthetic nested message with two fields
			// (key + value) and MapEntry = true. Build that shape by
			// hand.
			strType := descriptorpb.FieldDescriptorProto_TYPE_STRING
			labelOpt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			labelRep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
			valKind := kind
			entryName := "MEntry"
			entryTypeName := ".synthetic.Probe." + entryName
			file := &descriptorpb.FileDescriptorProto{
				Name:    proto.String("synthetic_map.proto"),
				Package: proto.String("synthetic"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{{
					Name: proto.String("Probe"),
					Field: []*descriptorpb.FieldDescriptorProto{{
						Name:     proto.String("m"),
						Number:   proto.Int32(1),
						Label:    &labelRep,
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: &entryTypeName,
					}},
					NestedType: []*descriptorpb.DescriptorProto{{
						Name: proto.String(entryName),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:   proto.String("key"),
								Number: proto.Int32(1),
								Label:  &labelOpt,
								Type:   &strType,
							},
							{
								Name:   proto.String("value"),
								Number: proto.Int32(2),
								Label:  &labelOpt,
								Type:   &valKind,
							},
						},
						Options: &descriptorpb.MessageOptions{
							MapEntry: proto.Bool(true),
						},
					}},
				}},
			}
			fd, err := protodesc.NewFile(file, nil)
			require.NoError(t, err)
			msg := fd.Messages().Get(0)
			field := msg.Fields().Get(0)
			require.True(t, field.IsMap())

			pfield := &protogen.Field{Desc: field}
			col := &storagev1.Column{}
			c := &ColumnPlan{}
			err = inferFieldEncoding(pfield, col, c)
			require.Errorf(t, err, "map<string,%s> must be rejected", kind)
			assert.Contains(t, err.Error(), "not persistable",
				"error must explain the value kind isn't landed yet")
		})
	}
}

// TestCheckRejectsEmptyString pins the narrow substring match used by
// the StringKind default-alignment guard. The match is intentionally
// conservative: only the literal "<col> <> <empty-string>" form and
// its swap. Predicates that semantically forbid the empty string but
// don't contain that substring (e.g. length(col) > 0) leave the
// inferred default in place; the plugin doesn't try to evaluate
// arbitrary CHECK expressions.
func TestCheckRejectsEmptyString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		check, dbName string
		want          bool
	}{
		{"", "name", false},
		{"name <> ''", "name", true},
		{"name<>''", "name", true},
		{"'' <> name", "name", true},
		{"''<>name", "name", true},
		{"length(name) > 0", "name", false}, // semantically rejects '' but not the literal substring
		{"name <> ''", "agent_name", false}, // wrong column
		{"agent_name <> ''", "agent_name", true},
		{"description <> '' AND name <> ''", "name", true}, // multi-part check, name still matches
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, checkRejectsEmptyString(c.check, c.dbName),
			"check=%q dbName=%q", c.check, c.dbName)
	}
}

// TestInferFieldEncoding_StringDefault — the StringKind branch emits
// the empty-string default only when no user-supplied check rejects
// it. Explicit default_expr always wins, regardless of the check.
func TestInferFieldEncoding_StringDefault(t *testing.T) {
	t.Parallel()
	field := stringFieldDescriptor(t)

	cases := []struct {
		name        string
		col         *storagev1.Column
		wantDefault string
	}{
		{
			name:        "no check, no default → DEFAULT ''",
			col:         &storagev1.Column{},
			wantDefault: "''",
		},
		{
			name:        "check rejects '' → no DEFAULT",
			col:         &storagev1.Column{Check: "name <> ''"},
			wantDefault: "",
		},
		{
			name:        "explicit default wins over <> '' check",
			col:         &storagev1.Column{Check: "name <> ''", DefaultExpr: "'placeholder'"},
			wantDefault: "'placeholder'",
		},
		{
			name:        "non-empty check that doesn't reject '' → DEFAULT ''",
			col:         &storagev1.Column{Check: "length(name) <= 63"},
			wantDefault: "''",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pfield := &protogen.Field{Desc: field}
			c := &ColumnPlan{
				DBName:      "name",
				DefaultExpr: tc.col.GetDefaultExpr(),
				Check:       tc.col.GetCheck(),
			}
			require.NoError(t, inferFieldEncoding(pfield, tc.col, c))
			assert.Equal(t, tc.wantDefault, c.DefaultExpr)
		})
	}
}

// TestInferFieldEncoding_EnumDefault — the EnumKind branch emits
// DEFAULT '<ZERO_NAME>' only when allow_zero_enum: true AND the
// caller didn't supply their own CHECK. Without allow_zero_enum, no
// inferred default. Explicit default_expr always wins.
func TestInferFieldEncoding_EnumDefault(t *testing.T) {
	t.Parallel()
	field := enumFieldDescriptor(t)

	cases := []struct {
		name        string
		col         *storagev1.Column
		wantDefault string
		wantCheck   string
	}{
		{
			name:        "no allow_zero_enum, no user check → no DEFAULT, CHECK excludes zero",
			col:         &storagev1.Column{},
			wantDefault: "",
			wantCheck:   "state IN ('STATE_RUNNING','STATE_STOPPED')",
		},
		{
			name:        "allow_zero_enum + no user check → DEFAULT 'STATE_UNSPECIFIED'",
			col:         &storagev1.Column{AllowZeroEnum: true},
			wantDefault: "'STATE_UNSPECIFIED'",
			wantCheck:   "state IN ('STATE_UNSPECIFIED','STATE_RUNNING','STATE_STOPPED')",
		},
		{
			name:        "allow_zero_enum + user check → no inferred DEFAULT",
			col:         &storagev1.Column{AllowZeroEnum: true, Check: "state IN ('STATE_RUNNING')"},
			wantDefault: "",
			wantCheck:   "state IN ('STATE_RUNNING')",
		},
		{
			name:        "explicit default_expr always wins",
			col:         &storagev1.Column{AllowZeroEnum: true, DefaultExpr: "'STATE_RUNNING'"},
			wantDefault: "'STATE_RUNNING'",
			wantCheck:   "state IN ('STATE_UNSPECIFIED','STATE_RUNNING','STATE_STOPPED')",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pfield := &protogen.Field{
				Desc: field,
				Enum: &protogen.Enum{Desc: field.Enum()},
			}
			c := &ColumnPlan{
				DBName:      "state",
				DefaultExpr: tc.col.GetDefaultExpr(),
				Check:       tc.col.GetCheck(),
			}
			require.NoError(t, inferFieldEncoding(pfield, tc.col, c))
			assert.Equal(t, tc.wantDefault, c.DefaultExpr, "DefaultExpr")
			assert.Equal(t, tc.wantCheck, c.Check, "Check")
		})
	}
}

// stringFieldDescriptor builds a synthetic `string name = 1` field via
// protodesc — enough for inferFieldEncoding to dispatch to StringKind.
func stringFieldDescriptor(t *testing.T) protoreflect.FieldDescriptor {
	t.Helper()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	kind := descriptorpb.FieldDescriptorProto_TYPE_STRING
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("synthetic_string.proto"),
		Package: proto.String("synthetic"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Probe"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("name"),
				Number: proto.Int32(1),
				Label:  &label,
				Type:   &kind,
			}},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	require.NoError(t, err)
	return fd.Messages().Get(0).Fields().Get(0)
}

// enumFieldDescriptor builds a synthetic `State state = 1` field where
// State has STATE_UNSPECIFIED = 0, STATE_RUNNING = 1, STATE_STOPPED = 2.
// Sufficient for inferFieldEncoding's EnumKind path.
func enumFieldDescriptor(t *testing.T) protoreflect.FieldDescriptor {
	t.Helper()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	kind := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	enumTypeName := ".synthetic.State"
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("synthetic_enum.proto"),
		Package: proto.String("synthetic"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("State"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("STATE_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("STATE_RUNNING"), Number: proto.Int32(1)},
				{Name: proto.String("STATE_STOPPED"), Number: proto.Int32(2)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Probe"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("state"),
				Number:   proto.Int32(1),
				Label:    &label,
				Type:     &kind,
				TypeName: &enumTypeName,
			}},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	require.NoError(t, err)
	return fd.Messages().Get(0).Fields().Get(0)
}
