package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	storagev1 "github.com/redpanda-data/protoc-gen-go-jet/gen/go/gojet/v1"
)

// resolveResource builds a ResourcePlan for a single message, or returns
// (nil, nil) when the message is not opted into persistence.
func resolveResource(f *protogen.File, m *protogen.Message, repoRoot, modulePath string) (*ResourcePlan, error) {
	table, ok := messageTable(m)
	if !ok {
		return nil, nil
	}
	if table.GetName() == "" {
		return nil, errors.New("table option missing name")
	}
	if err := validateSQLIdent(table.GetName()); err != nil {
		return nil, fmt.Errorf("table name %q: %w", table.GetName(), err)
	}

	plan := &ResourcePlan{
		Message:      m,
		ProtoFile:    f.Desc.Path(),
		ResourceFQN:  string(m.Desc.FullName()),
		TableName:    table.GetName(),
		TableComment: normaliseProtoComment(string(m.Comments.Leading)),
		PrimaryKey:   table.GetPrimaryKey(),
		RepoRoot:     repoRoot,
		ProtoGoIdent: m.GoIdent,
		ProtoGoName:  m.GoIdent.GoName,
	}

	// Tenancy
	if tt := table.GetTenancy(); tt != nil {
		tp := &TenancyPlan{
			Column:      valOr(tt.GetColumn(), "tenant_id"),
			RuntimeRole: tt.GetRuntimeRole(),
			UserScoped:  tt.GetUserScoped(),
			UserColumn:  tt.GetUserColumn(),
		}
		if tp.RuntimeRole == "" {
			return nil, errors.New("tenancy.runtime_role is required when tenancy is set")
		}
		if tp.UserScoped && tp.UserColumn == "" {
			tp.UserColumn = "user_id"
		}
		if err := validateSQLIdent(tp.Column); err != nil {
			return nil, fmt.Errorf("tenancy.column %q: %w", tp.Column, err)
		}
		if tp.UserScoped {
			if err := validateSQLIdent(tp.UserColumn); err != nil {
				return nil, fmt.Errorf("tenancy.user_column %q: %w", tp.UserColumn, err)
			}
		}
		plan.Tenancy = tp

		// Synthesize storage-owned columns.
		plan.Columns = append(plan.Columns, ColumnPlan{
			DBName:       tp.Column,
			SQLType:      "TEXT",
			NotNull:      true,
			Kind:         KindSynthesized,
			Synthesized:  true,
			JetFieldName: jetFieldNameFromDB(tp.Column),
			JetGoType:    "string",
		})
		if tp.UserScoped {
			plan.Columns = append(plan.Columns, ColumnPlan{
				DBName:       tp.UserColumn,
				SQLType:      "TEXT",
				NotNull:      true,
				Kind:         KindSynthesized,
				Synthesized:  true,
				JetFieldName: jetFieldNameFromDB(tp.UserColumn),
				JetGoType:    "string",
			})
		}
	}

	// Fields -> columns. Errors include the field's own source
	// location (field:line:col) so editor error-linking lands on
	// the bad annotation, not on the enclosing message.
	for _, field := range m.Fields {
		// Fields belonging to a real oneof are handled via oneof
		// resolution — the entire oneof maps to a <base>_kind + <base>
		// pair, not per-variant columns. If the author ALSO put a
		// `(storage.v1.column)` on a oneof variant, the annotation is
		// silently ignored today — a footgun worth refusing. Real
		// callers who wanted per-variant columns would have declared
		// them outside the oneof.
		if field.Oneof != nil && !field.Oneof.Desc.IsSynthetic() {
			if _, has := fieldColumn(field); has {
				return nil, fmt.Errorf("%s: field %s inside oneof %q carries (storage.v1.column), but oneof variants persist through the oneof's `<name>_kind` + `<name>` pair — drop the field-level column annotation; use `(storage.v1.oneof_column)` on the oneof to opt into persistence",
					sourceLoc(field.Desc), field.Desc.Name(), field.Oneof.Desc.Name())
			}
			continue
		}
		col, err := resolveColumn(field)
		if err != nil {
			return nil, fmt.Errorf("%s: field %s: %w", sourceLoc(field.Desc), field.Desc.Name(), err)
		}
		if col == nil {
			continue
		}
		plan.Columns = append(plan.Columns, *col)
	}

	// Oneofs
	for _, o := range m.Oneofs {
		if o.Desc.IsSynthetic() {
			continue
		}
		oc, err := resolveOneof(o)
		if err != nil {
			return nil, fmt.Errorf("%s: oneof %s: %w", sourceLoc(o.Desc), o.Desc.Name(), err)
		}
		if oc == nil {
			continue
		}
		plan.OneofColumns = append(plan.OneofColumns, *oc)
	}

	// Indexes
	for _, idx := range table.GetIndexes() {
		ip, err := resolveIndex(plan.TableName, idx)
		if err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
		plan.Indexes = append(plan.Indexes, ip)
	}

	// Partition-by — optional. Column-subset-of-PK is enforced in
	// validatePlan once `known` is materialised.
	if pb := table.GetPartitionBy(); pb != nil {
		pp, err := resolvePartition(pb)
		if err != nil {
			return nil, fmt.Errorf("partition_by: %w", err)
		}
		plan.Partition = pp
	}

	// Custom SQL — the declarative escape hatch. Each entry is one
	// raw statement; strip trailing whitespace + semicolons so the
	// emitter can always add exactly one semicolon + newline without
	// producing doubles. Empty entries (after trim) are dropped —
	// blank lines in a repeated string option shouldn't produce
	// spurious statement separators.
	for _, stmt := range table.GetCustomSql() {
		// Trim whitespace + any number of trailing semicolons; a
		// caller writing `"stmt;"` or `"stmt ;; "` and our renderer
		// appending another `;` would produce `stmt;;` in DDL.
		trimmed := strings.TrimRight(stmt, "; \t\r\n")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" {
			continue
		}
		plan.CustomSQL = append(plan.CustomSQL, trimmed)
	}

	// Ordering
	if s := strings.TrimSpace(table.GetDefaultOrderBy()); s != "" {
		parts, err := parseOrderBy(s)
		if err != nil {
			return nil, fmt.Errorf("default_order_by: %w", err)
		}
		plan.DefaultOrder = parts
	}
	if s := strings.TrimSpace(table.GetTieBreaker()); s != "" {
		parts, err := parseOrderBy(s)
		if err != nil {
			return nil, fmt.Errorf("tie_breaker: %w", err)
		}
		plan.TieBreaker = parts
	}

	// Output: every field auto-derives from the proto's Go package.
	// The plugin expects jet to emit under a fixed convention
	// (<proto_pkg>/storage/jet/public/{model,table}) so codegen paths
	// don't have to be repeated per resource. Overrides exist on
	// Output for the rare case a project wants a different layout.
	out := table.GetOutput()

	// Map the proto package's Go import path to a repo-relative output
	// directory by stripping the consumer's module path (read from
	// <repoRoot>/go.mod). This keeps the plugin module-agnostic: it works
	// for any module name, not just the one it was authored in.
	protoGoPkg := string(m.GoIdent.GoImportPath)
	if protoGoPkg != modulePath && !strings.HasPrefix(protoGoPkg, modulePath+"/") {
		return nil, fmt.Errorf("proto Go package %q is not under module %q (from go.mod) — set the proto's go_package (or buf managed go_package_prefix) so generated storage code lands inside your module", protoGoPkg, modulePath)
	}
	protoGoPkgRel := strings.TrimPrefix(protoGoPkg, modulePath+"/")

	// Flat layout: every annotated message in a proto package lands in
	// the SAME Go package (<proto_pkg>/storage), mirroring
	// protoc-gen-go's one-package-per-proto convention. Filenames are
	// prefixed by resource (llmprovider_mapper.go, mcpserver_mapper.go,
	// ...). Exported symbols are prefixed by the proto message name
	// (LLMProviderModel, LLMProviderTable, ...) to avoid collisions
	// inside the shared package.
	autoGoDir := filepath.ToSlash(filepath.Join(protoGoPkgRel, "storage"))
	autoJetModel := protoGoPkg + "/storage/jet/public/model"
	autoJetTable := protoGoPkg + "/storage/jet/public/table"

	plan.GoDir = valOr(out.GetGoDir(), autoGoDir)
	plan.GoImportPath = modulePath + "/" + plan.GoDir
	plan.GoPackageName = valOr(out.GetGoPackageName(), "storage")

	// FilenamePrefix is derived from the message name, not the table
	// name. Deriving it from table name would require an English plural
	// heuristic (strip trailing 's') that breaks on irregular nouns
	// (status → statu). Message names are singular and ASCII by
	// protobuf convention, so lowercasing them is both safe and stable.
	plan.SymbolPrefix = string(m.Desc.Name())                // "LLMProvider", "MCPServer", ...
	plan.FilenamePrefix = strings.ToLower(plan.SymbolPrefix) // "llmprovider", "mcpserver", ...
	plan.JetModelImport = valOr(out.GetJetModelImport(), autoJetModel)
	plan.JetTableImport = valOr(out.GetJetTableImport(), autoJetTable)
	plan.JetStructName = goJetStructName(plan.TableName)

	if err := validatePlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// requireColumnInPK enforces the "scope column must appear in the PK
// to avoid global uniqueness" rule used by both the tenant and the
// user-scoped tenant checks. `scopePlural` and `scopeSingular` name
// the conceptual grouping that would be exposed by the leak (e.g.
// "tenants" / "tenant", "users within the same tenant" / "user").
func requireColumnInPK(pk []string, col, field, scopePlural, scopeSingular string) error {
	for _, c := range pk {
		if c == col {
			return nil
		}
	}
	return fmt.Errorf("primary_key %v does not include %s %q — the PK would enforce uniqueness globally across %s, so a duplicate-key error on INSERT would leak the existence of another %s's row. Add %q to primary_key, or use Table.custom_sql for a table that deliberately wants global uniqueness", pk, field, col, scopePlural, scopeSingular, col)
}

// requireColumnInIndex is the index equivalent of requireColumnInPK:
// a non-partial UNIQUE index on a tenant-scoped table whose columns
// don't include the scope column (tenant or user) enforces uniqueness
// globally across that scope, exposing another tenant's/user's
// existence via the collision error.
func requireColumnInIndex(idx IndexPlan, col, field, scopePlural, scopeSingular string) error {
	for _, c := range idx.Columns {
		if c == col {
			return nil
		}
	}
	return fmt.Errorf("unique index %q on tenant-scoped table has columns %v which omit %s %q — uniqueness would be enforced globally across %s, so a duplicate-key error on INSERT would leak the existence of another %s's row. Add %q to the index columns, use `unique: true` on a Column (which auto-scopes to (tenant_id, col)), or declare a partial index via `where:` if the global scope is deliberate", idx.Name, idx.Columns, field, col, scopePlural, scopeSingular, col)
}

// validatePlan catches the "typo surface" — identifiers declared in one
// option but referencing columns elsewhere. Runs after the plan is fully
// resolved so it operates on the merged view (proto columns +
// synthesized tenant/user columns + oneof pair columns). Caught here
// rather than at migration time (PG rejecting an index on a ghost
// column) or at runtime (aipjet.Execute exploding on an unknown field
// path). Pure function of the plan so unit-tests don't need a protogen
// fixture.
func validatePlan(plan *ResourcePlan) error {
	known := map[string]bool{}
	synthesized := map[string]bool{}
	orderable := map[string]bool{}
	for _, c := range plan.Columns {
		if known[c.DBName] {
			// Most collisions are two proto fields collapsing to the
			// same snake_case name, but a proto field can also collide
			// with a plugin-synthesised column (tenant_id / user_id).
			// Emit a message that names the actual mechanism so the
			// author reaches for the right fix.
			if synthesized[c.DBName] && !c.Synthesized {
				return fmt.Errorf("column name %q on proto field collides with the plugin-synthesised tenancy column of the same name; rename the proto column via `(storage.v1.column).name`, or change `tenancy.column` / `tenancy.user_column` on the Table option", c.DBName)
			}
			return fmt.Errorf("column name %q appears twice — two proto fields collapse to the same snake_case name; set `name:` on one of the column options to disambiguate", c.DBName)
		}
		known[c.DBName] = true
		if c.Synthesized {
			synthesized[c.DBName] = true
		}
		if c.Orderable || c.Synthesized {
			// Synthesized tenant_id / user_id are always orderable
			// for tie-breakers even if not flagged explicitly.
			orderable[c.DBName] = true
		}
	}
	for _, oc := range plan.OneofColumns {
		if known[oc.KindColumn] {
			return fmt.Errorf("oneof %q kind column %q collides with an existing column", oc.BaseName, oc.KindColumn)
		}
		if known[oc.JSONColumn] {
			return fmt.Errorf("oneof %q JSON column %q collides with an existing column", oc.BaseName, oc.JSONColumn)
		}
		known[oc.KindColumn] = true
		known[oc.JSONColumn] = true
	}
	if len(plan.PrimaryKey) == 0 {
		return errors.New("primary_key is required — AIP-158 cursor pagination needs stable ordering anchored on a primary key subset")
	}
	for _, pk := range plan.PrimaryKey {
		if !known[pk] {
			return fmt.Errorf("primary_key references unknown column %q (declare it via a Column option or tenancy)", pk)
		}
	}
	// A tenant-scoped table whose PK doesn't include the tenant
	// column enforces *global* uniqueness across tenants. Two
	// tenants racing to insert the same row collide on the PK;
	// the losing tenant sees a duplicate-key error that exposes
	// cross-tenant state. Every tenant-scoped resource here includes
	// `tenant_id` as the first PK component. Make the convention
	// enforceable.
	//
	// For user-scoped tables the same leak applies one level deeper:
	// omitting `user_id` lets two users *within the same tenant*
	// collide, and the loser's INSERT error reveals another user's
	// existence. Require both columns when user-scoped.
	if plan.Tenancy != nil {
		if err := requireColumnInPK(plan.PrimaryKey, plan.Tenancy.Column, "tenancy.column", "tenants", "tenant"); err != nil {
			return err
		}
		if plan.Tenancy.UserScoped {
			if err := requireColumnInPK(plan.PrimaryKey, plan.Tenancy.UserColumn, "tenancy.user_column", "users within the same tenant", "user"); err != nil {
				return err
			}
		}
	}
	// Postgres requires every partition-key column to also appear in
	// the primary key; CREATE TABLE fails otherwise. Enforce here so
	// the failure is caught at generation, not at migration apply.
	if plan.Partition != nil {
		pkSet := make(map[string]bool, len(plan.PrimaryKey))
		for _, c := range plan.PrimaryKey {
			pkSet[c] = true
		}
		for _, c := range plan.Partition.Columns {
			if !known[c] {
				return fmt.Errorf("partition_by references unknown column %q", c)
			}
			if !pkSet[c] {
				return fmt.Errorf("partition_by column %q is not in primary_key — Postgres rejects partition keys that aren't a subset of the primary key", c)
			}
		}
	}
	indexNames := map[string]bool{}
	for _, idx := range plan.Indexes {
		if indexNames[idx.Name] {
			return fmt.Errorf("index name %q appears twice — two indexes have the same column list or the same explicit `name:`; disambiguate by setting `name:` on one", idx.Name)
		}
		indexNames[idx.Name] = true
		for _, col := range idx.Columns {
			if !known[col] {
				return fmt.Errorf("index %q references unknown column %q", idx.Name, col)
			}
		}
		// Same footgun as the PK case: a UNIQUE index on a tenant-
		// scoped table whose columns don't include the tenant column
		// enforces uniqueness globally across tenants. The collision
		// error at INSERT time leaks cross-tenant state.
		//
		// Partial indexes (`where: ...`) are a deliberate exception:
		// callers reach for them specifically to carve out a unique
		// subset (e.g. "one active provider per tenant" via
		// `where: "status = 'active'"` paired with `tenant_id` in
		// columns). A partial index without tenant_id in columns is
		// the author opting into a global uniqueness window — still
		// valid, just intentional. The error above already catches
		// the common-case bulk UNIQUE; the partial-index lane stays
		// author-owned.
		if idx.Unique && plan.Tenancy != nil && idx.Where == "" {
			if err := requireColumnInIndex(idx, plan.Tenancy.Column, "tenancy.column", "tenants", "tenant"); err != nil {
				return err
			}
			if plan.Tenancy.UserScoped {
				if err := requireColumnInIndex(idx, plan.Tenancy.UserColumn, "tenancy.user_column", "users within the same tenant", "user"); err != nil {
					return err
				}
			}
		}
	}
	// jsonb_indexed_paths on columns + oneofs produce implicit
	// expression indexes. Their synthesised names need to be unique
	// across the whole table — PG's CREATE INDEX would otherwise
	// fail at apply time and pg-schema-diff would flail. Catch at
	// generation instead.
	for _, c := range plan.Columns {
		seen := map[string]bool{}
		for _, path := range c.JSONBIndexedPaths {
			if seen[path] {
				return fmt.Errorf("column %q declares jsonb_indexed_paths path %q twice", c.DBName, path)
			}
			seen[path] = true
			name := jsonbPathIndexName(plan.TableName, c.DBName, path)
			if err := checkSynthIdentLen("jsonb_indexed_paths index", name); err != nil {
				return err
			}
			if indexNames[name] {
				return fmt.Errorf("jsonb_indexed_paths on %q produces index name %q which collides with an existing index — rename the colliding index explicitly or change the path", c.DBName, name)
			}
			indexNames[name] = true
		}
	}
	for _, oc := range plan.OneofColumns {
		seen := map[string]bool{}
		for _, path := range oc.JSONBIndexedPaths {
			if seen[path] {
				return fmt.Errorf("oneof %q declares jsonb_indexed_paths path %q twice", oc.BaseName, path)
			}
			seen[path] = true
			name := jsonbPathIndexName(plan.TableName, oc.JSONColumn, path)
			if err := checkSynthIdentLen("oneof jsonb_indexed_paths index", name); err != nil {
				return err
			}
			if indexNames[name] {
				return fmt.Errorf("jsonb_indexed_paths on oneof %q produces index name %q which collides with an existing index", oc.BaseName, name)
			}
			indexNames[name] = true
		}
	}
	// GIN + unique indexes go through the same name-uniqueness gate.
	// An explicit Table.indexes entry whose name happens to match a
	// synthesized GIN or unique name would fail at apply time; catch
	// it here with a clearer message.
	for _, c := range plan.Columns {
		if c.JSONBGinIndex {
			name := fmt.Sprintf("idx_%s_%s_gin", plan.TableName, c.DBName)
			if err := checkSynthIdentLen("jsonb_gin_index", name); err != nil {
				return err
			}
			if indexNames[name] {
				return fmt.Errorf("jsonb_gin_index on %q produces index name %q which collides with an existing index", c.DBName, name)
			}
			indexNames[name] = true
		}
		if c.Unique {
			// A unique index on the sole primary-key column would be
			// redundant — the PK constraint already enforces + indexes
			// uniqueness on exactly that column. Composite PKs are
			// different: `unique: true` on any member still adds value
			// (enforces uniqueness on that column independently of
			// the others).
			if len(plan.PrimaryKey) == 1 && plan.PrimaryKey[0] == c.DBName {
				return fmt.Errorf("unique on %q is redundant — column is the sole primary key, which already enforces and indexes uniqueness; drop `unique: true`", c.DBName)
			}
			name := fmt.Sprintf("idx_%s_%s_unique", plan.TableName, c.DBName)
			if err := checkSynthIdentLen("unique index", name); err != nil {
				return err
			}
			if indexNames[name] {
				return fmt.Errorf("unique on %q produces index name %q which collides with an existing index", c.DBName, name)
			}
			indexNames[name] = true
		}
		// CHECK constraint name is `<table>_<col>_check`. Inline
		// column CHECKs trip the same limit if table + column names
		// are long.
		if c.Check != "" {
			name := fmt.Sprintf("%s_%s_check", plan.TableName, c.DBName)
			if err := checkSynthIdentLen("CHECK constraint", name); err != nil {
				return err
			}
		}
	}
	// Oneof CHECK constraint names follow `<table>_<base>_kind_valid`.
	for _, oc := range plan.OneofColumns {
		name := fmt.Sprintf("%s_%s_kind_valid", plan.TableName, oc.BaseName)
		if err := checkSynthIdentLen("oneof kind CHECK", name); err != nil {
			return err
		}
	}
	for _, part := range plan.DefaultOrder {
		if !known[part.FieldPath] {
			return fmt.Errorf("default_order_by references unknown column %q", part.FieldPath)
		}
		if !orderable[part.FieldPath] {
			return fmt.Errorf("default_order_by references %q which is not orderable; set `orderable: true` on the column option or remove it from default_order_by", part.FieldPath)
		}
	}
	for _, part := range plan.TieBreaker {
		if !known[part.FieldPath] {
			return fmt.Errorf("tie_breaker references unknown column %q", part.FieldPath)
		}
		if !orderable[part.FieldPath] {
			return fmt.Errorf("tie_breaker references %q which is not orderable; a tie-breaker must be in the aipjet schema, so mark the column `orderable: true`", part.FieldPath)
		}
	}
	return nil
}

func resolveColumn(field *protogen.Field) (*ColumnPlan, error) {
	col, hasAnnotation := fieldColumn(field)
	// Persist-by-default: a field without `(storage.v1.column)` lands
	// in the DDL with inferred options, same as `(storage.v1.column) = {}`.
	// Fields opt OUT with `(storage.v1.column) = { skip: true }`. The
	// opt-in model silently dropped forgotten annotations — fields
	// vanished from the DDL with no feedback. Flipping the default
	// makes skipped fields visible in the proto text.
	if !hasAnnotation {
		col = &storagev1.Column{}
	}
	if col.GetSkip() {
		// AIP-203 IDENTIFIER + storage skip is contradictory: the
		// field IS the resource identifier, but we're told not to
		// store it. Either the identifier isn't stored (rare — the
		// resource can't round-trip its own identity through the
		// DB) or the author confused the two annotations. Refuse
		// loudly so the author fixes the intent before it ships.
		if impliesImmutable(field) {
			return nil, fmt.Errorf("field %s: `skip: true` conflicts with `(google.api.field_behavior) = IDENTIFIER | IMMUTABLE` — the behavior annotation says this is the resource identifier (or an immutable attribute), which contradicts excluding it from storage. Drop one of the two annotations", field.Desc.Name())
		}
		// Every other column option is nonsensical alongside skip:
		// orderable / immutable / unique / check / default_expr /
		// generated_expr / ... all describe HOW the column persists,
		// which doesn't apply when the column isn't persisted. The
		// plugin silently discarded them before — surface the
		// author's confusion loudly instead so the proto expresses
		// a single, coherent intent.
		if opt := otherSkipViolation(col); opt != "" {
			return nil, fmt.Errorf("field %s: `skip: true` set alongside `%s` — skip excludes the column from storage entirely, which makes other column options meaningless. Drop one of the two", field.Desc.Name(), opt)
		}
		return nil, nil
	}

	c := &ColumnPlan{
		Field:          field,
		ProtoFieldName: string(field.Desc.Name()),
		Orderable:      col.GetOrderable(),
		Immutable:      col.GetImmutable() || impliesImmutable(field),
		Nullable:       col.GetNullable(),
		Comment:        normaliseProtoComment(string(field.Comments.Leading)),
	}
	c.DBName = valOr(col.GetName(), toSnake(string(field.Desc.Name())))
	if err := validateSQLIdent(c.DBName); err != nil {
		return nil, fmt.Errorf("column name %q on field %q: %w", c.DBName, field.Desc.Name(), err)
	}
	c.NotNull = !col.GetNullable()
	c.DefaultExpr = col.GetDefaultExpr()
	c.Check = col.GetCheck()
	// `default_expr` and `check` are expression-shaped SQL — a
	// semicolon would end the enclosing statement prematurely
	// (either truncating CREATE TABLE or, more worryingly, allowing
	// a trailing statement to execute). Real expressions never need
	// them. Reject cleanly.
	if strings.Contains(c.DefaultExpr, ";") {
		return nil, fmt.Errorf("default_expr on field %q must not contain ';' — it's a SQL expression, not a statement; contains %q", field.Desc.Name(), c.DefaultExpr)
	}
	if strings.Contains(c.Check, ";") {
		return nil, fmt.Errorf("check on field %q must not contain ';' — it's a SQL expression, not a statement; contains %q", field.Desc.Name(), c.Check)
	}
	c.JetFieldName = jetFieldNameFromDB(c.DBName)

	// userDefault captures whether the caller set an explicit
	// default_expr, so applyNullable can clear the kind's auto-default
	// (which would override the desired NULL-on-omission) without
	// stomping an explicit one.
	userDefault := col.GetDefaultExpr()
	if err := inferFieldEncoding(field, col, c); err != nil {
		return nil, err
	}

	// Collection kinds (repeated / map / JSONB-list) default to nullable
	// so the proto-side nil vs. empty distinction survives a storage
	// round-trip. The opt-out is setting an explicit default_expr — the
	// generated DDL then carries NOT NULL + that default, matching the
	// legacy always-non-nil behavior for callers who want it.
	if userDefault == "" && defaultsToNullable(c.Kind) {
		c.Nullable = true
	}

	// inferFieldEncoding can promote Nullable itself — wrapper types
	// (google.protobuf.StringValue, ...) map to nullable native columns
	// regardless of `nullable:` on the option. Sync NotNull after so
	// the DDL emitter and mapper see a consistent view.
	c.NotNull = !c.Nullable

	if c.Nullable {
		if err := applyNullable(c, userDefault); err != nil {
			return nil, err
		}
	}

	if old := col.GetRenameFrom(); old != "" {
		if err := validateSQLIdent(old); err != nil {
			return nil, fmt.Errorf("rename_from %q on field %q: %w", old, field.Desc.Name(), err)
		}
		if old == c.DBName {
			return nil, fmt.Errorf("rename_from %q on field %q matches the current column name; drop the annotation if the rename has already landed", old, field.Desc.Name())
		}
		c.RenameFrom = old
	}

	if paths := col.GetJsonbIndexedPaths(); len(paths) > 0 {
		if !isJSONBKind(c.Kind) {
			return nil, fmt.Errorf("jsonb_indexed_paths is only valid on JSONB-backed columns (got %s for field %q)", c.Kind, field.Desc.Name())
		}
		for _, p := range paths {
			if err := validateJSONBPath(p); err != nil {
				return nil, fmt.Errorf("jsonb_indexed_paths %q on field %q: %w", p, field.Desc.Name(), err)
			}
		}
		c.JSONBIndexedPaths = paths
	}
	if col.GetJsonbGinIndex() {
		if !isJSONBKind(c.Kind) {
			return nil, fmt.Errorf("jsonb_gin_index is only valid on JSONB-backed columns (got %s for field %q)", c.Kind, field.Desc.Name())
		}
		c.JSONBGinIndex = true
	}
	if col.GetUnique() {
		if err := validateUniqueIndexable(c); err != nil {
			return nil, fmt.Errorf("unique on field %q: %w", field.Desc.Name(), err)
		}
		c.Unique = true
	}
	if fk := col.GetForeignKey(); fk != nil {
		// Reject kinds PG can't back an FK on: JSONB / arrays / bool.
		// PG would fail later at ADD CONSTRAINT with a cryptic "data
		// type X has no default operator class for access method
		// btree" or a type-mismatch error; catching here points at
		// the offending field in the proto. Mirrors the unique:true
		// guard so the two surfaces reject the same column kinds.
		if err := validateFKIndexable(c); err != nil {
			return nil, fmt.Errorf("foreign_key on field %q: %w", field.Desc.Name(), err)
		}
		// Parse here so malformed targets surface their source
		// location alongside other column errors. Cross-plan lookup
		// (target table/column, tenant symmetry, PK-or-unique check)
		// runs after every message is resolved — see resolve_fk.go.
		raw, err := parseFKAnnotation(fk, c)
		if err != nil {
			return nil, fmt.Errorf("foreign_key on field %q: %w", field.Desc.Name(), err)
		}
		c.ForeignKey = raw
	}
	if err := applyGeneratedExpr(col, c, string(field.Desc.Name())); err != nil {
		return nil, err
	}
	return c, nil
}

// applyGeneratedExpr validates `generated_expr` against the rules PG
// enforces (no DEFAULT alongside GENERATED) and against the option's
// out-of-scope decisions (no `unique` / `foreign_key` — declare those
// on regular columns instead). Lifted out of resolveColumn so unit
// tests can exercise the rules without constructing a synthetic proto
// FieldDescriptor with the column option attached. fieldName is the
// proto field name, used verbatim in error messages so the author
// sees the offending field.
func applyGeneratedExpr(col *storagev1.Column, c *ColumnPlan, fieldName string) error {
	expr := col.GetGeneratedExpr()
	if expr == "" {
		return nil
	}
	// Same rationale as default_expr / check: a `;` would terminate
	// the enclosing CREATE TABLE statement and let a trailing
	// statement run unintended. Real generation expressions never
	// need it.
	if strings.Contains(expr, ";") {
		return fmt.Errorf("generated_expr on field %q must not contain ';' — it's a SQL expression, not a statement; contains %q", fieldName, expr)
	}
	// PG rejects DEFAULT alongside GENERATED ALWAYS AS ... STORED.
	// Catch the conflict here so the author sees a clear error
	// instead of a syntax failure at apply time.
	//
	// Read the user-supplied annotation, not the resolved
	// `c.DefaultExpr` — `inferFieldEncoding` has already auto-set a
	// kind-default (e.g. "0" for int64) by this point, and that
	// auto-default isn't a real conflict; the DDL emitter's switch
	// on GeneratedExpr already suppresses it.
	if col.GetDefaultExpr() != "" {
		return fmt.Errorf("field %q sets both generated_expr and default_expr — Postgres rejects DEFAULT alongside GENERATED; pick one", fieldName)
	}
	// Clear the resolved default so downstream consumers (DDL emit,
	// drift check, mapper) see a coherent state — a generated column
	// has no DEFAULT, period.
	c.DefaultExpr = ""
	// `unique` and `foreign_key` on a generated column are
	// technically legal in PG but out of scope for this option's
	// declarative surface. Force the author to declare the regular
	// column instead so the index / FK is unambiguously associated
	// with the underlying source columns.
	if col.GetUnique() {
		return fmt.Errorf("field %q sets both generated_expr and unique — declare the unique constraint on a regular column instead", fieldName)
	}
	if col.GetForeignKey() != nil {
		return fmt.Errorf("field %q sets both generated_expr and foreign_key — foreign keys on generated columns are out of scope; use a regular column", fieldName)
	}
	c.GeneratedExpr = expr
	return nil
}

// validateFKIndexable rejects column kinds PG can't back a foreign
// key on. PG requires FK columns to be comparable by btree equality
// against the referenced column's unique index, which rules out
// JSONB / arrays (no default btree opclass) and bool (FK to a bool
// unique is a modelling mistake — at most two parent rows).
//
// Mirrors validateUniqueIndexable; the two guards keep the same
// shape-set so `unique:` and `foreign_key` reject identically, which
// is the expected invariant given the FK is backed by a unique
// constraint on the target.
func validateFKIndexable(c *ColumnPlan) error {
	switch c.Kind {
	case KindScalar:
		if c.JetGoType == "bool" || c.JetGoType == "*bool" {
			return errors.New("bool columns are not FK-indexable — btree FKs on a two-valued column are almost always a modelling mistake")
		}
		return nil
	case KindTimestamp, KindDuration, KindEnumAsText:
		return nil
	}
	return fmt.Errorf("foreign_key is only valid on scalar / timestamp / duration / enum columns — got %s", c.Kind)
}

// validateUniqueIndexable rejects column kinds PG can't index as a
// plain btree. JSONB / TEXT[] / array kinds would need a special
// operator class; bool is two-valued so a unique constraint admits
// at most two rows and is almost certainly a modelling mistake.
func validateUniqueIndexable(c *ColumnPlan) error {
	switch c.Kind {
	case KindScalar:
		if c.JetGoType == "bool" || c.JetGoType == "*bool" {
			return errors.New("bool columns admit at most two distinct values — a unique index on a bool is almost always a mistake; use a partial index via `Table.indexes` if you really want this")
		}
		return nil
	case KindTimestamp, KindDuration, KindEnumAsText:
		return nil
	}
	return fmt.Errorf("unique is only valid on scalar / timestamp / duration / enum columns — got %s", c.Kind)
}

// isJSONBKind reports whether a ColumnKind stores its payload in a
// JSONB-typed column. Only those can carry jsonb_indexed_paths —
// emitting an expression index on a TEXT column would be a build-time
// error at CREATE INDEX.
func isJSONBKind(k ColumnKind) bool {
	switch k {
	case KindJSONBProto, KindJSONBProtoList, KindJSONBStrMap,
		KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage:
		return true
	}
	return false
}

// validateSQLIdent restricts table + column names to the snake_case
// subset of Postgres identifiers. Spec-legal identifiers are broader
// (mixed case, uppercase-after-first, even unicode with the right
// settings), but every name the plugin emits lands in CREATE TABLE
// / CREATE INDEX / RLS policy bodies unquoted. Quoting would work
// but would diverge from the repo-wide convention and surprise
// migration reviewers.
//
// Rejecting catches three real failure modes:
//   - SQL injection: `"drop users; --"` would embed verbatim into
//     the generated DDL.
//   - Silent case-folding: PG folds unquoted identifiers to
//     lowercase, so `"MyTable"` becomes `mytable` at apply time
//     but the plugin would emit `MyTable` in index names and
//     drift-check wouldn't match.
//   - Leading-digit / special-char issues that would fail at apply
//     time with a cryptic syntax error.
//
// Pattern: starts with a lowercase letter or underscore, then
// lowercase letters / digits / underscores. Matches the
// Postgres-convention identifier shape every plugin user follows.

// impliesImmutable returns true when a field carries
// `(google.api.field_behavior) = IDENTIFIER` or
// `(google.api.field_behavior) = IMMUTABLE`. AIP-203 defines both
// as "preserved by the server through Update", which is exactly the
// storage-layer immutable contract. Mirroring saves authors from
// writing the same intent twice: declaring `IDENTIFIER` already
// implies `immutable: true` at the DDL level.
//
// OUTPUT_ONLY does NOT imply immutable — `updated_at` is
// OUTPUT_ONLY (server populates it) but mutates on every update.
// Other field_behavior values are unrelated to storage and ignored.
func impliesImmutable(field *protogen.Field) bool {
	if field == nil || field.Desc == nil {
		return false
	}
	opts := field.Desc.Options()
	if opts == nil {
		return false
	}
	v := proto.GetExtension(opts, annotations.E_FieldBehavior)
	behaviors, ok := v.([]annotations.FieldBehavior)
	if !ok {
		return false
	}
	for _, b := range behaviors {
		if b == annotations.FieldBehavior_IDENTIFIER || b == annotations.FieldBehavior_IMMUTABLE {
			return true
		}
	}
	return false
}

// otherSkipViolation returns the name of the first non-skip column
// option that carries a meaningful value alongside `skip: true`, or
// "" if every other field is at its proto3 default. The plugin
// ignores every other option under skip (the column never lands in
// the DDL), so the pair is author confusion worth surfacing.
//
// proto3 booleans can't distinguish "unset" from "false", so this
// only detects combinations a reader would actually misread — any
// non-empty string, a non-UNSPECIFIED Storage enum, or a non-empty
// repeated list. Explicit `nullable: false` / `orderable: false`
// wouldn't flag (they look the same as unset) and are
// redundant-but-harmless.
func otherSkipViolation(col *storagev1.Column) string {
	if col.GetImmutable() {
		return "immutable: true"
	}
	if col.GetOrderable() {
		return "orderable: true"
	}
	if col.GetUnique() {
		return "unique: true"
	}
	if col.GetNullable() {
		return "nullable: true"
	}
	if col.GetAllowZeroEnum() {
		return "allow_zero_enum: true"
	}
	if col.GetJsonbGinIndex() {
		return "jsonb_gin_index: true"
	}
	if col.GetName() != "" {
		return "name"
	}
	if col.GetDefaultExpr() != "" {
		return "default_expr"
	}
	if col.GetCheck() != "" {
		return "check"
	}
	if col.GetRenameFrom() != "" {
		return "rename_from"
	}
	if col.GetStorage() != storagev1.Storage_STORAGE_UNSPECIFIED {
		return "storage"
	}
	if len(col.GetJsonbIndexedPaths()) > 0 {
		return "jsonb_indexed_paths"
	}
	if col.GetGeneratedExpr() != "" {
		return "generated_expr"
	}
	return ""
}

func validateSQLIdent(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	// Postgres truncates identifiers > 63 bytes (NAMEDATALEN - 1)
	// with a warning that's easy to miss. Two identifiers differing
	// only in their suffix past byte 63 would collide silently at
	// apply time. Refuse here.
	const pgNameMax = 63
	if len(s) > pgNameMax {
		return fmt.Errorf("exceeds Postgres NAMEDATALEN-1 (%d bytes); PG would truncate silently and risk collisions with similarly-named identifiers", pgNameMax)
	}
	for i, r := range s {
		switch {
		case i == 0 && (r == '_' || (r >= 'a' && r <= 'z')):
			// OK — leading lowercase letter or underscore.
		case i > 0 && (r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')):
			// OK — trailing lowercase alphanumeric or underscore.
		default:
			return fmt.Errorf("must match [a-z_][a-z0-9_]* (snake_case); got %q at position %d", r, i)
		}
	}
	return nil
}

// checkSynthIdentLen guards plugin-synthesised identifier names
// (index names, CHECK constraint names, oneof CHECK names) against
// Postgres's 63-byte identifier limit. User-supplied names already
// go through validateSQLIdent which applies the same cap. This
// helper catches the compound case where the inputs are each legal
// but the concatenated synthesis overshoots — e.g. a 30-byte
// table name + a 30-byte column name + a path component.
//
// `kind` describes the synthesis for the error message so the
// author knows which annotation to shorten.
func checkSynthIdentLen(kind, name string) error {
	const pgNameMax = 63
	if len(name) <= pgNameMax {
		return nil
	}
	return fmt.Errorf("plugin-synthesised %s name %q is %d bytes, exceeds Postgres NAMEDATALEN-1 (%d bytes) — shorten the table name, column name, or jsonb path that feeds the synthesis; PG would otherwise truncate silently and risk collisions", kind, name, len(name), pgNameMax)
}

// validateJSONBPath rejects shapes that would emit invalid SQL or
// silently index the wrong thing. Segments are restricted to
// `[a-zA-Z0-9_]` (case-insensitive — jsonbPathIndexName lowercases)
// because the path also feeds a synthesised PG index name that must
// be a legal unquoted identifier after folding. Real JSON keys
// outside this set (hyphens, UTF-8, dots) aren't blocked from being
// indexed — they're just outside the shorthand's scope; reach them
// via `Table.custom_sql` where the caller picks their own index
// name and can quote the identifier as needed.
func validateJSONBPath(p string) error {
	if p == "" {
		return errors.New("path must not be empty")
	}
	if strings.Contains(p, "'") || strings.Contains(p, "\"") {
		return errors.New("path must not contain quote characters")
	}
	// Backslash is context-dependent in PG: safe under the default
	// standard_conforming_strings=on, but treated as an escape inside
	// E'...' literals. The plugin emits plain `'...'` today, but a
	// future edit could regress to E-strings; reject backslashes now
	// so the emitted DDL is always session-independent.
	if strings.Contains(p, "\\") {
		return errors.New("path must not contain backslash characters")
	}
	// Control characters would break DDL formatting, confuse
	// pg-schema-diff's idempotent-plan parser, and in the case of
	// a null byte fail outright at apply (PG TEXT doesn't store 0x00).
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path must not contain control characters; got U+%04X", r)
		}
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			return errors.New("path segment must not be empty (stray dot?)")
		}
		// Each segment must be identifier-safe after lowercasing so
		// the synthesised index name stays a legal unquoted PG
		// identifier. Rejecting at validation is cleaner than
		// silently sanitising (which would fold `x-req` and `x.req`
		// and `x_req` into one index name — collision footgun).
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z',
				r >= 'A' && r <= 'Z',
				r >= '0' && r <= '9',
				r == '_':
				// OK
			default:
				return fmt.Errorf("path segment %q contains character %q; jsonb_indexed_paths is restricted to [a-zA-Z0-9_] per segment so the synthesised index name is a legal unquoted identifier — reach exotic keys via custom_sql", seg, r)
			}
		}
	}
	return nil
}

// wrapperNativeMapping describes the native-column encoding of a
// google.protobuf.*Value wrapper: the SQL type, the jet model Go type
// (pre-pointer — applyNullable promotes to pointer), and the wrapperspb
// constructor used to reconstruct the message on read.
type wrapperNativeMapping struct {
	sqlType string
	goType  string
	ctor    string
}

// wrapperNative returns the native-column mapping for a well-known
// wrapper type's full proto name, or nil if the message isn't a
// wrapper the plugin natively supports.
func wrapperNative(fullName string) *wrapperNativeMapping {
	switch fullName {
	case "google.protobuf.StringValue":
		return &wrapperNativeMapping{sqlType: "TEXT", goType: "string", ctor: "String"}
	case "google.protobuf.BoolValue":
		return &wrapperNativeMapping{sqlType: "BOOLEAN", goType: "bool", ctor: "Bool"}
	case "google.protobuf.Int32Value":
		return &wrapperNativeMapping{sqlType: "INTEGER", goType: "int32", ctor: "Int32"}
	case "google.protobuf.UInt32Value":
		return &wrapperNativeMapping{sqlType: "INTEGER", goType: "int32", ctor: "UInt32"}
	case "google.protobuf.Int64Value":
		return &wrapperNativeMapping{sqlType: "BIGINT", goType: "int64", ctor: "Int64"}
	case "google.protobuf.UInt64Value":
		return &wrapperNativeMapping{sqlType: "BIGINT", goType: "int64", ctor: "UInt64"}
	case "google.protobuf.BytesValue":
		return &wrapperNativeMapping{sqlType: "BYTEA", goType: "[]byte", ctor: "Bytes"}
	case "google.protobuf.FloatValue":
		return &wrapperNativeMapping{sqlType: "REAL", goType: "float32", ctor: "Float"}
	case "google.protobuf.DoubleValue":
		return &wrapperNativeMapping{sqlType: "DOUBLE PRECISION", goType: "float64", ctor: "Double"}
	}
	return nil
}

// defaultsToNullable reports whether a ColumnKind defaults to nullable
// at the DDL layer. Collection kinds (repeated primitive arrays, JSONB
// list, JSONB maps) have a meaningful nil-vs-empty distinction on the
// proto side — a nil map/slice is "absent", an empty one is "present
// but empty". Flattening both to the same storage shape (NOT NULL
// DEFAULT '{}') would discard that distinction. Making the column
// nullable by default preserves it: nil ↔ SQL NULL ↔ pointer nil, and
// empty ↔ '{}' ↔ non-nil empty collection.
//
// Callers opt back into NOT NULL + empty-literal default by setting
// `default_expr` on the column option.
func defaultsToNullable(k ColumnKind) bool {
	switch k {
	case KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindJSONBProtoList,
		KindJSONBStrMap, KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage:
		return true
	}
	return false
}

// applyNullable post-processes a plan to represent the NULL state at
// each layer:
//
//   - NotNull is already cleared by resolveColumn; the DDL emitter
//     skips NOT NULL automatically.
//   - The jet model field type is promoted to its pointer form so
//     go-jet's introspection matches what the generator emits. Bytes
//     is the lone exception — []byte already distinguishes nil from
//     empty natively.
//   - DefaultExpr is cleared (unless the caller set one explicitly)
//     so the column defaults to NULL rather than the kind's zero-
//     value literal (”, 0, ...).
//   - Proto-side presence must be representable. Scalars need proto3
//     `optional`; message-valued fields track presence via nil pointer
//     naturally. Repeated / map / JSONB-list kinds track presence via
//     the slice/map's own nil state; no `optional` needed.
func applyNullable(c *ColumnPlan, userDefault string) error {
	// Allow-list, not deny-list — adding a new ColumnKind must be an
	// explicit opt-in to nullable rather than a silent pointerise
	// that goes to production.
	switch c.Kind {
	case KindScalar, KindEnumAsText, KindDuration:
		// Wrappers (google.protobuf.*Value) are KindScalar but track
		// presence through the wrapper message's pointer, not via
		// proto3 `optional`. Only non-wrapper scalars/enums/durations
		// need the `optional` keyword.
		if c.WrapperCtor == "" && (c.Field == nil || !c.Field.Desc.HasOptionalKeyword()) {
			return fmt.Errorf("field %s: nullable scalar requires proto3 `optional` — without it the mapper can't tell unset from zero", c.Field.Desc.Name())
		}
	case KindTimestamp:
		// google.protobuf.Timestamp has presence via the pointer getter.
	case KindJSONBProto:
		// Single nested message — presence via the pointer getter, like
		// KindTimestamp. A nil message persists as SQL NULL; a present
		// message (even an all-default {}) persists as JSONB. This is the
		// only way a JSONB-proto column distinguishes "absent" (NULL, e.g.
		// a never-reported status) from "present but empty" ({}); the
		// default NOT-NULL "{}"-sentinel mapping collapses the two.
	case KindRepeatedText, KindRepeatedEnum, KindRepeatedBool,
		KindRepeatedInt32, KindRepeatedInt64,
		KindRepeatedFloat32, KindRepeatedFloat64,
		KindRepeatedTimestamp, KindRepeatedDuration,
		KindJSONBProtoList,
		KindJSONBStrMap, KindJSONBMapScalar, KindJSONBMapEnum, KindJSONBMapMessage:
		// Collection kinds — the slice/map's own nil signals absence.
		// No proto3 optional needed.
	default:
		name := protoreflect.Name("<synthesized>")
		if c.Field != nil {
			name = c.Field.Desc.Name()
		}
		return fmt.Errorf("field %s: nullable is not supported on this kind — synthesized columns are storage-owned and have no proto presence to map", name)
	}

	// Every nullable jet column maps to a pointer model field — jet's
	// model generator emits *string / *int32 / *time.Time / *[]byte
	// for any column without NOT NULL. For columns with a plugin-
	// supplied type override (e.g. jettypes.TimestampArray), jetgen's
	// customiseSchema hook prepends the `*` so the model field matches
	// what the mapper emits here.
	c.JetGoType = "*" + c.JetGoType
	if userDefault == "" {
		c.DefaultExpr = ""
	}
	return nil
}

// checkRejectsEmptyString reports whether a user-supplied CHECK
// expression obviously forbids the empty string for the column. The
// match is intentionally narrow — `<dbname> <> ”` and the swapped
// `” <> <dbname>` form, with optional whitespace — so we don't try
// to evaluate arbitrary CHECK expressions; a richer predicate that
// happens to also reject ” will leave the inferred default in place.
func checkRejectsEmptyString(check, dbName string) bool {
	if check == "" {
		return false
	}
	for _, pattern := range []string{
		dbName + " <> ''",
		dbName + "<>''",
		"'' <> " + dbName,
		"''<>" + dbName,
	} {
		if strings.Contains(check, pattern) {
			return true
		}
	}
	return false
}

// inferFieldEncoding fills Kind, SQLType, JetGoType, JetGoImport
// entirely from the proto field shape plus the Storage override (which
// picks among proto-side representations — STORAGE_JSONB_STRMAP vs
// STORAGE_ARRAY for map fields, etc.). The SQL type itself is not
// overridable: the proto kind is the single source of truth, so the
// mapper and go-jet introspection always agree on Go types.
// validateStorageOverride refuses storage hints that don't match
// the field's proto shape. Before this guard, mismatched hints
// parsed silently and the plugin picked the inferred kind anyway —
// leaving authors convinced their override took effect until they
// looked at the emitted DDL. Rejecting forces the author to either
// remove a no-op annotation or change the proto shape to match the
// stored representation they actually want.
//
// STORAGE_UNSPECIFIED is the default and means "use the inferred
// kind" — always legal.
func validateStorageOverride(field *protogen.Field, override storagev1.Storage) error {
	if override == storagev1.Storage_STORAGE_UNSPECIFIED {
		return nil
	}
	isMap := field.Desc.IsMap()
	isRepeated := field.Desc.IsList()
	kind := field.Desc.Kind()
	switch override {
	case storagev1.Storage_STORAGE_JSONB_STRMAP:
		if !isMap {
			return fmt.Errorf("field %s: storage=JSONB_STRMAP requires a map<string,string> field — the hint exists only to force the StrMap fast path when the shape matches", field.Desc.Name())
		}
		keyKind := field.Desc.MapKey().Kind()
		valKind := field.Desc.MapValue().Kind()
		if keyKind != protoreflect.StringKind || valKind != protoreflect.StringKind {
			return fmt.Errorf("field %s: storage=JSONB_STRMAP requires map<string,string> — got map<%s,%s>. Drop the override; the generic map encoding handles every legal proto3 key/value combination", field.Desc.Name(), keyKind, valKind)
		}
	case storagev1.Storage_STORAGE_ARRAY:
		if !isRepeated || isMap {
			return fmt.Errorf("field %s: storage=ARRAY requires a `repeated` scalar / enum / well-known field — drop the override, or change the proto shape to `repeated <scalar>`", field.Desc.Name())
		}
	case storagev1.Storage_STORAGE_TEXT_ENUM:
		if kind != protoreflect.EnumKind {
			return fmt.Errorf("field %s: storage=TEXT_ENUM requires a proto enum field — TEXT+CHECK is the enum-specific encoding; drop the override for non-enum kinds", field.Desc.Name())
		}
	case storagev1.Storage_STORAGE_JSONB_PROTO:
		if kind != protoreflect.MessageKind || isMap {
			return fmt.Errorf("field %s: storage=JSONB_PROTO requires a message-kind field — the override exists to force protojson encoding on nested messages; drop it for scalars, enums, and maps", field.Desc.Name())
		}
	}
	return nil
}

func inferFieldEncoding(field *protogen.Field, col *storagev1.Column, c *ColumnPlan) error {
	override := col.GetStorage()
	if err := validateStorageOverride(field, override); err != nil {
		return err
	}

	// Repeated fields.
	if field.Desc.IsList() {
		switch field.Desc.Kind() {
		case protoreflect.StringKind:
			c.Kind = KindRepeatedText
			c.SQLType = "TEXT[]"
			c.JetGoType = "pq.StringArray"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.EnumKind:
			c.Kind = KindRepeatedEnum
			c.SQLType = "TEXT[]"
			c.JetGoType = "pq.StringArray"
			c.JetGoImport = "github.com/lib/pq"
			// No auto-CHECK on array elements — Postgres lacks clean per-element
			// CHECK. If you need it, add a hand-written constraint in a
			// follow-up migration.
		case protoreflect.BoolKind:
			c.Kind = KindRepeatedBool
			c.SQLType = "BOOLEAN[]"
			c.JetGoType = "pq.BoolArray"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
			c.Kind = KindRepeatedInt32
			c.SQLType = "INTEGER[]"
			c.JetGoType = "pq.Int32Array"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			c.Kind = KindRepeatedInt64
			c.SQLType = "BIGINT[]"
			c.JetGoType = "pq.Int64Array"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.FloatKind:
			c.Kind = KindRepeatedFloat32
			c.SQLType = "REAL[]"
			c.JetGoType = "pq.Float32Array"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.DoubleKind:
			c.Kind = KindRepeatedFloat64
			c.SQLType = "DOUBLE PRECISION[]"
			c.JetGoType = "pq.Float64Array"
			c.JetGoImport = "github.com/lib/pq"
		case protoreflect.MessageKind:
			// Timestamp rides `TIMESTAMPTZ[]` with a custom named type
			// (`jettypes.TimestampArray`) because pq ships no time-
			// array scanner. The override lands in the jet model via
			// `jetgen.ColumnOverrides` (wired from main.go's
			// `jetColumnOverrides`), so the model field type matches
			// what the mapper emits here without manual editing.
			//
			// Duration stays as BIGINT[] nanoseconds — PG has no
			// DURATION type, and Duration scalars already persist as
			// BIGINT, so the repeated case is consistent with the
			// scalar case.
			if msg := field.Message.Desc; msg != nil {
				switch string(msg.FullName()) {
				case "google.protobuf.Timestamp":
					c.Kind = KindRepeatedTimestamp
					c.SQLType = "TIMESTAMPTZ[]"
					c.JetGoType = "jettypes.TimestampArray"
					c.JetGoImport = "github.com/redpanda-data/protoc-gen-go-jet/pkg/pgstore/jettypes"
					return nil
				case "google.protobuf.Duration":
					c.Kind = KindRepeatedDuration
					c.SQLType = "BIGINT[]"
					c.JetGoType = "pq.Int64Array"
					c.JetGoImport = "github.com/lib/pq"
					return nil
				}
			}
			// Repeated non-well-known messages land in JSONB (array).
			c.Kind = KindJSONBProtoList
			c.SQLType = "JSONB"
			c.JetGoType = "string"
			return nil
		case protoreflect.BytesKind:
			// `repeated bytes` would need a hand-written BYTEA[] scanner
			// (pq ships none). Tracked in TODO.md.
			return errors.New("repeated bytes not supported — BYTEA[] needs a custom scanner; tracked in TODO.md")
		default:
			return fmt.Errorf("repeated %s not supported", field.Desc.Kind())
		}
		// Repeated primitive kinds default to nullable — no DEFAULT
		// emitted. defaultsToNullable + applyNullable handle the pointer
		// promotion downstream. Callers who want NOT NULL DEFAULT '{}'
		// set `default_expr: "'{}'"` on the field's column option.
		return nil
	}

	// Maps.
	if field.Desc.IsMap() {
		return inferMapEncoding(field, override, c)
	}

	// Scalar kinds.
	switch field.Desc.Kind() {
	case protoreflect.StringKind:
		c.Kind = KindScalar
		c.SQLType = "TEXT"
		c.JetGoType = "string"
		// Default alignment: emit DEFAULT '' only when no user check
		// obviously rejects the empty string. A check that contains
		// `<col> <> ''` (e.g. on identifier columns) would make the
		// inferred default invalid at insert time. An explicit
		// default_expr captured at the top of resolve always wins.
		if c.DefaultExpr == "" && !checkRejectsEmptyString(c.Check, c.DBName) {
			c.DefaultExpr = "''"
		}
	case protoreflect.BoolKind:
		c.Kind = KindScalar
		c.SQLType = "BOOLEAN"
		c.JetGoType = "bool"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "false"
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		c.Kind = KindScalar
		c.SQLType = "INTEGER"
		c.JetGoType = "int32"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "0"
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		c.Kind = KindScalar
		c.SQLType = "BIGINT"
		c.JetGoType = "int64"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "0"
		}
	case protoreflect.BytesKind:
		c.Kind = KindScalar
		c.SQLType = "BYTEA"
		c.JetGoType = "[]byte"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "''::bytea"
		}
	case protoreflect.FloatKind:
		c.Kind = KindScalar
		c.SQLType = "REAL"
		c.JetGoType = "float32"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "0"
		}
	case protoreflect.DoubleKind:
		c.Kind = KindScalar
		c.SQLType = "DOUBLE PRECISION"
		c.JetGoType = "float64"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "0"
		}
	case protoreflect.EnumKind:
		c.Kind = KindEnumAsText
		c.SQLType = "TEXT"
		c.JetGoType = "string"
		// Auto-CHECK from enum values unless caller supplied an explicit
		// check. The zero value (by convention <NAME>_UNSPECIFIED) is
		// excluded by default — in proto3 the zero value is
		// indistinguishable from "field not set", so persisting it is
		// almost always a caller bug. Opt in via allow_zero_enum when
		// the zero value has defined semantics.
		if c.Check == "" {
			names := []string{}
			zeroName := ""
			values := field.Enum.Desc.Values()
			for i := 0; i < values.Len(); i++ {
				v := values.Get(i)
				if v.Number() == 0 {
					zeroName = string(v.Name())
					if !col.GetAllowZeroEnum() {
						continue
					}
				}
				names = append(names, fmt.Sprintf("'%s'", v.Name()))
			}
			if len(names) == 0 {
				return fmt.Errorf("enum %s has no persistable values (only the zero value is defined) — set allow_zero_enum: true or add real variants", field.Enum.Desc.FullName())
			}
			c.Check = fmt.Sprintf("%s IN (%s)", c.DBName, strings.Join(names, ","))
			// Default alignment: only emit an inferred default when the
			// plugin can prove it satisfies the auto-CHECK it just
			// built. With allow_zero_enum, the zero value is in the
			// CHECK list, so it's a valid default; otherwise no
			// default — callers must supply the value on insert,
			// matching proto3 "zero is unset". An explicit default_expr
			// captured earlier in resolve always wins.
			if c.DefaultExpr == "" && col.GetAllowZeroEnum() && zeroName != "" {
				c.DefaultExpr = fmt.Sprintf("'%s'", zeroName)
			}
		}
		// When the caller supplied their own CHECK we cannot prove
		// what values are admitted, so we emit no inferred default. An
		// explicit default_expr (captured at the top of resolve) is
		// still preserved.
	case protoreflect.MessageKind:
		if msg := field.Message.Desc; msg != nil {
			switch string(msg.FullName()) {
			case "google.protobuf.Timestamp":
				c.Kind = KindTimestamp
				c.SQLType = "TIMESTAMPTZ"
				c.JetGoType = "time.Time"
				return nil
			case "google.protobuf.Duration":
				// Store as BIGINT nanoseconds. Postgres INTERVAL would
				// be semantically closer but loses sub-microsecond
				// precision and complicates go-jet's default scan
				// path. Nanoseconds fit durations up to ~292 years.
				c.Kind = KindDuration
				c.SQLType = "BIGINT"
				c.JetGoType = "int64"
				if c.DefaultExpr == "" {
					c.DefaultExpr = "0"
				}
				return nil
			}
			// Primitive wrapper types (StringValue, Int32Value, ...)
			// route to a nullable native column. Presence on the proto
			// side is the wrapper pointer's nilness; unset → SQL NULL,
			// set → nullable scalar. Beats the JSONB-per-field fallback
			// — saves storage + makes the value queryable in SQL.
			if wrapper := wrapperNative(string(msg.FullName())); wrapper != nil {
				c.Kind = KindScalar
				c.SQLType = wrapper.sqlType
				c.JetGoType = wrapper.goType
				c.WrapperCtor = wrapper.ctor
				c.Nullable = true
				// Skip the auto-default so the column is NULL-by-default.
				return nil
			}
		}
		// Any other nested message -> JSONB proto.
		c.Kind = KindJSONBProto
		c.SQLType = "JSONB"
		c.JetGoType = "string"
		if c.DefaultExpr == "" {
			c.DefaultExpr = "'{}'::jsonb"
		}
	default:
		return fmt.Errorf("unsupported field kind %s", field.Desc.Kind())
	}
	return nil
}

// inferMapEncoding routes a map<K,V> field to one of three JSONB Kinds.
// Fast path: map<string,string> → KindJSONBStrMap (legacy, byte-identical
// output to pre-existing deployments). Scalar V → KindJSONBMapScalar
// (stdlib json covers both sides natively; proto int/bool keys stringify
// via encoding/json's TextMarshaler path — for proto3 key kinds that
// means strconv on the integer key and "true"/"false" for bool). Message
// V → KindJSONBMapMessage (per-value protojson wrapped in
// json.RawMessage to frame the outer object).
//
// Float/double V are rejected — the scalar kinds for those aren't landed
// yet (see README's "Planned" section). The natural upstream rejection
// for illegal map key kinds (messages, repeated, map, float, double) is
// enforced by protoreflect, so no explicit key-validation here.
func inferMapEncoding(field *protogen.Field, override storagev1.Storage, c *ColumnPlan) error {
	keyKind := field.Desc.MapKey().Kind()
	valField := field.Desc.MapValue()
	valKind := valField.Kind()

	c.MapKeyKind = keyKind
	c.MapValueKind = valKind

	// All map kinds default to nullable — the proto-side nil-vs-empty
	// distinction survives a round-trip. Callers opt back into NOT NULL
	// DEFAULT '{}'::jsonb by setting `default_expr` on the column.

	// Preserve the existing byte-identical fast path. STORAGE_JSONB_STRMAP
	// is the explicit override; the implicit fast path is pure
	// map<string,string>. Anything else falls through to the generic kinds
	// below, which handle every legal proto3 map shape.
	if override == storagev1.Storage_STORAGE_JSONB_STRMAP ||
		(keyKind == protoreflect.StringKind && valKind == protoreflect.StringKind) {
		c.Kind = KindJSONBStrMap
		c.SQLType = "JSONB"
		c.JetGoType = "string"
		return nil
	}

	// Message value → per-element protojson framed by json.RawMessage.
	if valKind == protoreflect.MessageKind {
		c.Kind = KindJSONBMapMessage
		c.SQLType = "JSONB"
		c.JetGoType = "string"
		return nil
	}

	// Scalar value (string / bool / int / bytes / enum). stdlib json handles
	// all of them natively on both sides: protogen-generated Go types for
	// int/bool keys satisfy encoding/json's TextMarshaler/TextUnmarshaler
	// (encoding via their default formatters in json); bytes values
	// round-trip as base64 strings. Enum values surface as int32, which
	// is lossy for enum-name round-trips — reject cleanly rather than
	// silently downgrading.
	switch valKind {
	case protoreflect.StringKind, protoreflect.BoolKind, protoreflect.BytesKind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		c.Kind = KindJSONBMapScalar
		c.SQLType = "JSONB"
		c.JetGoType = "string"
		return nil
	case protoreflect.EnumKind:
		// Store each enum value as its `String()` name, matching how
		// bare enum scalars persist via KindEnumAsText. Keeps names
		// stable across enum renumbering and makes the stored JSON
		// self-describing. Parsing back goes through <Enum>_value[].
		c.Kind = KindJSONBMapEnum
		c.SQLType = "JSONB"
		c.JetGoType = "string"
		return nil
	}
	return fmt.Errorf("map<%s,%s>: value kind %s is not persistable", keyKind, valKind, valKind)
}

func resolveOneof(o *protogen.Oneof) (*OneofColumnPlan, error) {
	opt, ok := oneofColumn(o)
	if !ok {
		return nil, nil
	}
	base := valOr(opt.GetName(), toSnake(string(o.Desc.Name())))
	if err := validateSQLIdent(base); err != nil {
		return nil, fmt.Errorf("oneof %q base column name %q: %w", o.Desc.Name(), base, err)
	}

	plan := &OneofColumnPlan{
		BaseName:     base,
		KindColumn:   base + "_kind",
		JSONColumn:   base,
		Comment:      normaliseProtoComment(string(o.Comments.Leading)),
		Optional:     opt.GetOptional(),
		Oneof:        o,
		JetKindField: jetFieldNameFromDB(base + "_kind"),
		JetJSONField: jetFieldNameFromDB(base),
	}
	for _, f := range o.Fields {
		switch f.Desc.Kind() {
		case protoreflect.MessageKind,
			protoreflect.BoolKind,
			protoreflect.StringKind,
			protoreflect.BytesKind,
			protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
			protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
			protoreflect.FloatKind, protoreflect.DoubleKind,
			protoreflect.EnumKind:
			// OK — message, scalar, or enum. All land in the same
			// JSONB column; the `_kind` discriminator drives dispatch.
		case protoreflect.GroupKind:
			return nil, fmt.Errorf("variant %s: proto2 groups are not supported", f.Desc.Name())
		default:
			return nil, fmt.Errorf("variant %s: unsupported kind %s", f.Desc.Name(), f.Desc.Kind())
		}
		plan.Variants = append(plan.Variants, OneofVariant{
			VariantName: toSnake(string(f.Desc.Name())),
			Field:       f,
		})
	}
	if len(plan.Variants) == 0 {
		return nil, errors.New("oneof has no variants")
	}
	if paths := opt.GetJsonbIndexedPaths(); len(paths) > 0 {
		for _, p := range paths {
			if err := validateJSONBPath(p); err != nil {
				return nil, fmt.Errorf("oneof %s jsonb_indexed_paths %q: %w", o.Desc.Name(), p, err)
			}
		}
		plan.JSONBIndexedPaths = paths
	}
	return plan, nil
}

func resolveIndex(table string, idx *storagev1.Index) (IndexPlan, error) {
	cols := idx.GetColumns()
	if len(cols) == 0 {
		return IndexPlan{}, errors.New("index must list at least one column")
	}
	// PG rejects duplicate columns in the same index at CREATE time
	// ("column ... specified more than once"). A typo that repeats a
	// column name would otherwise only surface at apply, late in the
	// migration lifecycle. Catch at generation.
	seenCol := make(map[string]bool, len(cols))
	for _, c := range cols {
		if seenCol[c] {
			return IndexPlan{}, fmt.Errorf("index lists column %q more than once — PG rejects duplicate columns in the same index", c)
		}
		seenCol[c] = true
	}
	ord := idx.GetOrder()
	if len(ord) > len(cols) {
		return IndexPlan{}, fmt.Errorf("index has %d order entries for only %d columns — trailing entries would be silently dropped", len(ord), len(cols))
	}
	ip := IndexPlan{
		Columns: cols,
		Unique:  idx.GetUnique(),
		Where:   idx.GetWhere(),
	}
	// `where` is an expression-shaped SQL predicate; a `;` would
	// terminate the enclosing CREATE INDEX prematurely.
	if strings.Contains(ip.Where, ";") {
		return IndexPlan{}, fmt.Errorf("index where clause must not contain ';' — it's a SQL predicate, not a statement; contains %q", ip.Where)
	}
	ip.Name = valOr(idx.GetName(), fmt.Sprintf("idx_%s_%s", table, strings.Join(cols, "_")))
	if err := validateSQLIdent(ip.Name); err != nil {
		return IndexPlan{}, fmt.Errorf("index name %q: %w", ip.Name, err)
	}
	for i, col := range cols {
		if i < len(ord) && ord[i] != "" {
			up := strings.ToUpper(ord[i])
			if up != "ASC" && up != "DESC" {
				return IndexPlan{}, fmt.Errorf("index column %s: order must be ASC or DESC, got %q", col, ord[i])
			}
			ip.Order = append(ip.Order, up)
		} else {
			ip.Order = append(ip.Order, "ASC")
		}
	}
	return ip, nil
}

// resolvePartition validates and lowers the `partition_by` option. The
// column-subset-of-PK rule (Postgres rejects otherwise) runs later in
// validatePlan once synthesized columns are merged into the plan.
func resolvePartition(p *storagev1.Partition) (*PartitionPlan, error) {
	var method string
	switch p.GetMethod() {
	case storagev1.PartitionMethod_PARTITION_METHOD_RANGE:
		method = "RANGE"
	case storagev1.PartitionMethod_PARTITION_METHOD_LIST:
		method = "LIST"
	case storagev1.PartitionMethod_PARTITION_METHOD_HASH:
		method = "HASH"
	case storagev1.PartitionMethod_PARTITION_METHOD_UNSPECIFIED:
		return nil, errors.New("method is required (RANGE, LIST, or HASH)")
	default:
		return nil, fmt.Errorf("unknown method %v", p.GetMethod())
	}
	cols := p.GetColumns()
	if len(cols) == 0 {
		return nil, errors.New("columns must list at least one column")
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		if seen[c] {
			return nil, fmt.Errorf("column %q listed more than once", c)
		}
		seen[c] = true
	}
	return &PartitionPlan{Method: method, Columns: cols}, nil
}

// parseOrderBy parses "field asc, other desc" into OrderPart slices.
func parseOrderBy(s string) ([]OrderPart, error) {
	parts := []OrderPart{}
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		tokens := strings.Fields(term)
		op := OrderPart{FieldPath: tokens[0]}
		switch {
		case len(tokens) == 1:
			// default asc
		case len(tokens) == 2 && strings.EqualFold(tokens[1], "asc"):
			// asc
		case len(tokens) == 2 && strings.EqualFold(tokens[1], "desc"):
			op.Desc = true
		default:
			return nil, fmt.Errorf("invalid order term %q (want \"field [asc|desc]\")", term)
		}
		parts = append(parts, op)
	}
	if len(parts) == 0 {
		return nil, errors.New("empty order expression")
	}
	return parts, nil
}

// messageTable reads the (storagev1.table) option off a message.
func messageTable(m *protogen.Message) (*storagev1.Table, bool) {
	opts := m.Desc.Options()
	if opts == nil {
		return nil, false
	}
	v := proto.GetExtension(opts, storagev1.E_Table)
	tbl, ok := v.(*storagev1.Table)
	if !ok || tbl == nil {
		return nil, false
	}
	return tbl, true
}

func fieldColumn(f *protogen.Field) (*storagev1.Column, bool) {
	opts := f.Desc.Options()
	if opts == nil {
		return nil, false
	}
	if !proto.HasExtension(opts, storagev1.E_Column) {
		return nil, false
	}
	v := proto.GetExtension(opts, storagev1.E_Column)
	col, ok := v.(*storagev1.Column)
	if !ok || col == nil {
		return nil, false
	}
	return col, true
}

func oneofColumn(o *protogen.Oneof) (*storagev1.OneofColumn, bool) {
	opts := o.Desc.Options()
	if opts == nil {
		return nil, false
	}
	if !proto.HasExtension(opts, storagev1.E_OneofColumn) {
		return nil, false
	}
	v := proto.GetExtension(opts, storagev1.E_OneofColumn)
	oc, ok := v.(*storagev1.OneofColumn)
	if !ok || oc == nil {
		return nil, false
	}
	return oc, true
}

func valOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// normaliseProtoComment turns a protogen leading comment (raw, includes
// leading space from "// ", newlines between comment lines) into a
// single-line string suitable for a SQL COMMENT ON literal. Empty
// input returns "", which the emitter uses to decide whether to emit
// the COMMENT at all.
func normaliseProtoComment(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
	}
	return b.String()
}

// toSnake converts camelCase / PascalCase to snake_case. Insert a
// separator before an uppercase rune when either the preceding rune is
// lowercase ("aA") or the following rune is lowercase ("AB[a]" —
// the last cap of a run that begins a new word). Proto identifiers are
// ASCII by spec, so byte-indexing with s[i-1]/s[i+1] is safe.
func toSnake(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && isUpper(r) {
			prevLower := isLower(rune(s[i-1]))
			nextLower := i+1 < len(s) && isLower(rune(s[i+1]))
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		b.WriteRune(toLower(r))
	}
	return b.String()
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func toLower(r rune) rune {
	if isUpper(r) {
		return r + ('a' - 'A')
	}
	return r
}

// jetFieldNameFromDB produces go-jet's model field name from a snake_case
// column name. Mirrors go-jet's internal snaker (commonInitialisms +
// snakeToCamelExceptions) so generated identifiers match what `jet` emits
// when it introspects the live DB.
func jetFieldNameFromDB(col string) string {
	return snakeToPascal(col)
}

// goJetStructName converts a snake_case table name to go-jet's PascalCase
// struct: "llm_providers" -> "LlmProviders", "oauth_providers" ->
// "OAuthProviders".
func goJetStructName(table string) string {
	return snakeToPascal(table)
}

// snakeToPascal implements the same camel-casing as jet's internal snaker:
// each snake-cased part is either an exception ("oauth" -> "OAuth"), a
// known all-caps initialism ("ID" -> "ID"), or a plain title-case word.
func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if e, ok := jetSnakeExceptions[p]; ok {
			b.WriteString(e)
			continue
		}
		up := strings.ToUpper(p)
		if jetAcronyms[up] {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// jetAcronyms mirrors go-jet's commonInitialisms at the version
// pinned in go.mod. Source of truth:
//
//	$GOPATH/pkg/mod/github.com/go-jet/jet/v2@<version>/internal/3rdparty/snaker/snaker.go
//
// Drift detection: when bumping go-jet, diff that file's
// `commonInitialisms` var against the entries below. Drift
// produces subtle mapper compile errors (`gen.Table.ID` vs
// `gen.Table.Id` when jet adds a new initialism) — easy to spot
// but non-obvious to diagnose. The snaker package is
// `internal/3rdparty`, so we can't import and compare at test
// time; a maintainer-visible sync reminder at upgrade is the
// cheapest reliable detection. As of go-jet v2.14.1 this list is
// byte-exact with jet's commonInitialisms.
var jetAcronyms = map[string]bool{
	"ACL":   true,
	"API":   true,
	"ASCII": true,
	"CPU":   true,
	"CSS":   true,
	"DNS":   true,
	"EOF":   true,
	"ETA":   true,
	"GPU":   true,
	"GUID":  true,
	"HTML":  true,
	"HTTP":  true,
	"HTTPS": true,
	"ID":    true,
	"IP":    true,
	"JSON":  true,
	"LHS":   true,
	"OS":    true,
	"QPS":   true,
	"RAM":   true,
	"RHS":   true,
	"RPC":   true,
	"SLA":   true,
	"SMTP":  true,
	"SQL":   true,
	"SSH":   true,
	"TCP":   true,
	"TLS":   true,
	"TTL":   true,
	"UDP":   true,
	"UI":    true,
	"UID":   true,
	"UUID":  true,
	"URI":   true,
	"URL":   true,
	"UTF8":  true,
	"VM":    true,
	"XML":   true,
	"XMPP":  true,
	"XSRF":  true,
	"XSS":   true,
}

// jetSnakeExceptions mirrors go-jet's snakeToCamelExceptions at
// the version pinned in go.mod. Same upgrade-time sync discipline
// as jetAcronyms above — if a go-jet bump adds a new exception
// and our mirror misses it, generated mappers reference fields
// that don't match jet's introspected struct (compile error).
// Byte-exact against go-jet v2.14.1.
var jetSnakeExceptions = map[string]string{
	"oauth": "OAuth",
}
