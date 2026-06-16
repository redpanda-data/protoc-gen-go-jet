package main

import (
	"errors"
	"fmt"
	"strings"

	storagev1 "github.com/redpanda-data/protoc-gen-go-jet/gen/go/gojet/v1"
)

// parseFKAnnotation reads the raw proto `foreign_key` option for one
// column and produces a half-resolved ForeignKeyPlan. Cross-message
// lookup happens later — see resolveForeignKeys — because the target
// may live in a different .proto file than the referring field, and
// protogen exposes files one at a time to the per-resource resolver.
//
// This pass covers everything local to the column: target-string
// well-formedness, default actions, and the caller's explicit
// BackingIndex override. Actions that conflict with the column's
// own shape (SET NULL on a NOT NULL column, SET DEFAULT without a
// default_expr) are also caught here — the check uses only the
// referring column and doesn't need the target.
func parseFKAnnotation(fk *storagev1.ForeignKey, c *ColumnPlan) (*ForeignKeyPlan, error) {
	target := strings.TrimSpace(fk.GetTarget())
	if target == "" {
		return nil, errors.New("target is required")
	}
	if err := validateFKTargetSyntax(target); err != nil {
		return nil, err
	}
	plan := &ForeignKeyPlan{
		RawTarget: target,
		OnDelete:  fkActionSQL(fk.GetOnDelete(), true),
		OnUpdate:  fkActionSQL(fk.GetOnUpdate(), false),
	}
	if fk.Index != nil {
		// Explicit caller override. The auto-suppression logic below
		// in resolveForeignKeys will not flip the value back.
		plan.BackingIndex = *fk.Index
	} else {
		// Default: emit. Auto-suppressed in the cross-plan pass when
		// the column is already the leading column of an existing
		// declared index.
		plan.BackingIndex = true
	}
	if err := validateFKActionsAgainstColumn(plan, c); err != nil {
		return nil, err
	}
	return plan, nil
}

// fkActionSQL maps the proto Action enum to Postgres clause body.
// Always returns a canonical spelling so two emitters produce
// byte-identical DDL and pg_get_constraintdef round-trips stably.
// NO ACTION is chosen for on_update default because PG's own default
// is NO ACTION; keeping it explicit makes the DDL readable without
// requiring the reader to remember Postgres defaults.
func fkActionSQL(a storagev1.Action, isDelete bool) string {
	switch a {
	case storagev1.Action_ACTION_UNSPECIFIED:
		if isDelete {
			return "RESTRICT"
		}
		return "NO ACTION"
	case storagev1.Action_ACTION_RESTRICT:
		return "RESTRICT"
	case storagev1.Action_ACTION_CASCADE:
		return "CASCADE"
	case storagev1.Action_ACTION_SET_NULL:
		return "SET NULL"
	case storagev1.Action_ACTION_SET_DEFAULT:
		return "SET DEFAULT"
	case storagev1.Action_ACTION_NO_ACTION:
		return "NO ACTION"
	}
	return "NO ACTION"
}

// validateFKActionsAgainstColumn rejects action/column combinations
// that would fail at CREATE CONSTRAINT time with cryptic Postgres
// errors. SET NULL needs a nullable column; SET DEFAULT needs a
// default expression. Catching here means the author fixes the
// intent in the proto instead of chasing a pg error in apply logs.
func validateFKActionsAgainstColumn(fk *ForeignKeyPlan, c *ColumnPlan) error {
	for _, pair := range []struct {
		action string
		label  string
	}{
		{fk.OnDelete, "on_delete"},
		{fk.OnUpdate, "on_update"},
	} {
		switch pair.action {
		case "SET NULL":
			if c.NotNull {
				return fmt.Errorf("%s: SET NULL requires a nullable column — mark the column `nullable: true` or choose a different action", pair.label)
			}
		case "SET DEFAULT":
			if c.DefaultExpr == "" {
				return fmt.Errorf("%s: SET DEFAULT requires `default_expr` on the column — set a default or choose a different action", pair.label)
			}
		}
	}
	return nil
}

// validateFKTargetSyntax checks the target string is syntactically
// `<Message>.<field>` or `.<fq.pkg>.<Message>.<field>`. We do not
// verify the target exists yet — that happens in the cross-plan pass
// where the registry is available.
func validateFKTargetSyntax(target string) error {
	if target == "" {
		return errors.New("target is required")
	}
	if strings.ContainsAny(target, " \t\n\r") {
		return fmt.Errorf("target %q must not contain whitespace", target)
	}
	lastDot := strings.LastIndex(target, ".")
	if lastDot <= 0 {
		return fmt.Errorf(`target %q: expected "<Message>.<field>" or ".pkg.Message.field"`, target)
	}
	field := target[lastDot+1:]
	if field == "" {
		return fmt.Errorf("target %q: missing field name after last '.'", target)
	}
	// Proto field names are `[a-z_][a-z0-9_]*` by proto3 convention —
	// the column generator already lowercases message names for snake-
	// case conversion but actual field identifiers are always lower.
	// Allow a deliberately permissive check here and defer stricter
	// rules to the cross-plan lookup (where we compare against the
	// actual protogen.Field name).
	for i, r := range field {
		switch {
		case r == '_',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9' && i > 0,
			// Proto3 technically allows digits after position 0, and
			// accepts upper-case in message names (but not field names).
			// We compare against Desc.Name() exactly in the lookup, so
			// permissiveness here does not risk silent false matches.
			r >= 'A' && r <= 'Z':
		default:
			return fmt.Errorf("target %q: field part %q contains invalid character %q", target, field, r)
		}
	}
	msgPart := target[:lastDot]
	if strings.HasPrefix(msgPart, ".") {
		trimmed := strings.TrimPrefix(msgPart, ".")
		if trimmed == "" || strings.HasSuffix(trimmed, ".") {
			return fmt.Errorf("target %q: fully-qualified part %q is malformed", target, msgPart)
		}
	} else if strings.Contains(msgPart, ".") {
		return fmt.Errorf("target %q: cross-package references must start with a leading dot (got %q)", target, msgPart)
	}
	return nil
}

// resolveForeignKeys is the cross-message pass. It runs after every
// resource in the current plugin run has been resolved, looks each
// FK target up in the registry of annotated messages, applies the
// tenant auto-composite rule, decides whether the backing index is
// needed, and injects supplemental UNIQUE constraints onto target
// tables where the PK does not already cover (tenant_id, ref_col).
//
// Running as a post-pass is necessary: a `.example.v1
// .LLMProvider.id` target in mcp_server.proto isn't reachable via the
// per-file protogen.File the resolver sees. Doing it here also keeps
// per-message resolution order-independent — the pass treats the
// plan set as a flat registry.
func resolveForeignKeys(plans []*ResourcePlan) error {
	byFQN := map[string]*ResourcePlan{}
	for _, p := range plans {
		byFQN[p.ResourceFQN] = p
	}
	var errs []error
	for _, p := range plans {
		for ci := range p.Columns {
			c := &p.Columns[ci]
			if c.ForeignKey == nil {
				continue
			}
			if err := completeFK(p, c, byFQN); err != nil {
				fieldName := c.DBName
				if c.Field != nil {
					fieldName = string(c.Field.Desc.Name())
				}
				errs = append(errs,
					fmt.Errorf("%s: %s: field %s: foreign_key: %w",
						columnSourceLoc(*c), p.ResourceFQN, fieldName, err))
			}
		}
	}
	if len(errs) > 0 {
		return joinErrs("foreign-key resolution failed", errs)
	}
	return nil
}

func completeFK(p *ResourcePlan, c *ColumnPlan, byFQN map[string]*ResourcePlan) error {
	fk := c.ForeignKey
	targetFQN, targetField := splitFKTarget(fk.RawTarget, p.ResourceFQN)
	target, ok := byFQN[targetFQN]
	if !ok {
		return fmt.Errorf("target %q: message %q not found among persisted resources in this generate run — ensure the target carries `(storage.v1.table)` and is in the same buf generate pass", fk.RawTarget, targetFQN)
	}
	targetCol := findColumnByProtoField(target, targetField)
	if targetCol == nil {
		return fmt.Errorf("target %q: message %s has no persisted field named %q — check for a typo or for `skip: true` on the target field", fk.RawTarget, target.TableName, targetField)
	}

	// Tenancy symmetry is checked before FK eligibility: an
	// asymmetric setup produces a specific, actionable error. If we
	// let eligibility fail first on a composite-PK target, the caller
	// would chase "not eligible" when the actual fix is "align the
	// tenancy or drop the FK annotation".
	selfTenant := p.Tenancy != nil
	targetTenant := target.Tenancy != nil
	if selfTenant != targetTenant {
		return fmt.Errorf("asymmetric tenancy: referring table %s %s tenancy, target table %s %s tenancy — align the two (promote or demote one) or use Table.custom_sql if the cross-scope reference is intentional",
			p.TableName, tenancyVerb(selfTenant),
			target.TableName, tenancyVerb(targetTenant))
	}

	// Eligibility: the plain-FK case requires caller opt-in (single-
	// column PK or `unique: true`) because silently injecting a
	// UNIQUE constraint onto a non-tenant target is a surprising
	// side-effect — it introduces a new cluster-wide uniqueness
	// invariant that affects every caller of that table.
	//
	// The tenant-composite case is different: the injected UNIQUE
	// is `(tenant_id, target_col)`, which tightens uniqueness only
	// within a tenant. For tenant-scoped data this is almost always
	// a reasonable invariant (IDs are per-tenant unique in practice),
	// so the plugin injects without requiring opt-in.
	if !selfTenant && !isFKEligibleTarget(target, targetCol) {
		return fmt.Errorf("target %q: field %q on %s is not FK-eligible — the target must be the single-column primary key of the target table or carry `unique: true`", fk.RawTarget, targetField, target.TableName)
	}

	fk.TargetTable = target.TableName
	fk.TargetColumn = targetCol.DBName
	fk.ConstraintName = fmt.Sprintf("%s_%s_fkey", p.TableName, c.DBName)

	if selfTenant {
		fk.TenantComposite = true
		fk.LocalTenantColumn = p.Tenancy.Column
		fk.TargetTenantColumn = target.Tenancy.Column
		// Inject a UNIQUE (tenant_id, target_col) on the target when
		// neither the PK nor an existing `unique: true` already covers
		// the pair. The `unique: true` path is the common case for
		// non-PK FK targets; checking it here avoids emitting a
		// redundant second UNIQUE index.
		if !targetPKCoversTenantComposite(target, targetCol.DBName) && !targetCol.Unique {
			addSupplementalUnique(target, SupplementalUniqueConstraint{
				Name:    fmt.Sprintf("uq_%s_%s_%s", target.TableName, target.Tenancy.Column, targetCol.DBName),
				Columns: []string{target.Tenancy.Column, targetCol.DBName},
			})
		}
	}

	if fk.BackingIndex {
		if columnAlreadyCovered(p, c.DBName) {
			// An index already covers this column as its leading key
			// — the FK would ride on the existing btree. Suppress the
			// auto-emit to avoid a duplicate redundant index.
			fk.BackingIndex = false
		} else {
			fk.IndexName = fmt.Sprintf("idx_%s_%s_fk", p.TableName, c.DBName)
			if err := checkSynthIdentLen("FK backing index", fk.IndexName); err != nil {
				return err
			}
		}
	}
	return checkSynthIdentLen("FK constraint", fk.ConstraintName)
}

// splitFKTarget splits a raw target into (messageFQN, fieldName). The
// caller supplies the current resource's FQN so a simple "Msg.field"
// form resolves to the same proto package. A leading-dot ".a.b.Msg.field"
// is taken verbatim after the dot is stripped.
func splitFKTarget(raw, selfFQN string) (string, string) {
	lastDot := strings.LastIndex(raw, ".")
	msgPart := raw[:lastDot]
	fieldPart := raw[lastDot+1:]
	if strings.HasPrefix(msgPart, ".") {
		return strings.TrimPrefix(msgPart, "."), fieldPart
	}
	// Simple name: qualify against the current resource's proto pkg.
	// selfFQN is `<pkg>.<Message>`; strip trailing `.<Message>`.
	dot := strings.LastIndex(selfFQN, ".")
	pkg := ""
	if dot > 0 {
		pkg = selfFQN[:dot]
	}
	if pkg == "" {
		return msgPart, fieldPart
	}
	return pkg + "." + msgPart, fieldPart
}

func findColumnByProtoField(p *ResourcePlan, protoFieldName string) *ColumnPlan {
	for i := range p.Columns {
		c := &p.Columns[i]
		if c.Synthesized {
			continue
		}
		if c.ProtoFieldName == protoFieldName {
			return c
		}
	}
	return nil
}

// isFKEligibleTarget enforces Postgres's "FK must reference a unique
// constraint" rule. Two shapes qualify: the column IS the single-
// column primary key, or the column carries `unique: true` (which
// the plugin will emit a UNIQUE index for).
//
// Note: a column inside a composite primary key does NOT qualify on
// its own — a composite PK enforces uniqueness of the tuple, not of
// any individual member. That's the whole point of the tenant auto-
// composite FK: caller references `id`, plugin expands to
// `(tenant_id, id)` which DOES match the PK.
func isFKEligibleTarget(p *ResourcePlan, c *ColumnPlan) bool {
	if len(p.PrimaryKey) == 1 && p.PrimaryKey[0] == c.DBName {
		return true
	}
	if c.Unique {
		return true
	}
	return false
}

// targetPKCoversTenantComposite reports whether the target's primary
// key is exactly (tenant_col, target_col) regardless of declared
// order — PG matches FK-referenced columns to a unique constraint by
// set, not sequence.
func targetPKCoversTenantComposite(target *ResourcePlan, targetCol string) bool {
	if target.Tenancy == nil {
		return false
	}
	if len(target.PrimaryKey) != 2 {
		return false
	}
	want := map[string]bool{target.Tenancy.Column: true, targetCol: true}
	for _, pk := range target.PrimaryKey {
		if !want[pk] {
			return false
		}
		delete(want, pk)
	}
	return len(want) == 0
}

// columnAlreadyCovered reports whether `col` is the leading column of
// an index that would make a separate FK backing index redundant. The
// scan is strict about "leading": a composite index on (a, b) covers
// lookups on a but not on b, because the btree is sorted first by a.
// Tenant-scoped unique columns automatically become leading columns
// of their own (tenant_id, col) index, which does NOT help an FK on
// col alone — so those do not count as coverage.
func columnAlreadyCovered(p *ResourcePlan, col string) bool {
	if len(p.PrimaryKey) > 0 && p.PrimaryKey[0] == col {
		return true
	}
	for _, idx := range p.Indexes {
		if len(idx.Columns) > 0 && idx.Columns[0] == col {
			return true
		}
	}
	for _, c := range p.Columns {
		if c.DBName != col {
			continue
		}
		// c.Unique on a non-tenant table emits `(col)`, which covers.
		// On a tenant-scoped table it emits `(tenant_id, col)` which
		// does not. See renderUniqueColumnIndexes for the emit rule.
		if c.Unique && p.Tenancy == nil {
			return true
		}
	}
	return false
}

// addSupplementalUnique de-duplicates by constraint name so multiple
// incoming FKs to the same (tenant_id, col) do not inject the same
// UNIQUE twice.
func addSupplementalUnique(p *ResourcePlan, uq SupplementalUniqueConstraint) {
	for _, existing := range p.SupplementalUniques {
		if existing.Name == uq.Name {
			return
		}
	}
	p.SupplementalUniques = append(p.SupplementalUniques, uq)
}

// tenancyVerb renders the clause fragment used inside asymmetric-
// tenancy errors. Having a helper keeps the error message symmetric
// and readable ("X has tenancy, Y lacks tenancy").
func tenancyVerb(has bool) string {
	if has {
		return "has"
	}
	return "lacks"
}
