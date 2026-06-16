package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJoinErrs_SingleUnwraps pins that a one-element list is still wrapped
// with errors.Is-compatible %w — callers can match the underlying error.
func TestJoinErrs_SingleUnwraps(t *testing.T) {
	inner := errors.New("boom")
	got := joinErrs("prefix", []error{inner})
	require.Error(t, got)
	assert.Equal(t, "prefix: boom", got.Error())
	assert.True(t, errors.Is(got, inner), "single-error path must preserve unwrap chain")
}

// TestJoinErrs_MultipleListsBullets checks that the >1 path produces the
// bullet-list format buf / protoc renders cleanly.
func TestJoinErrs_MultipleListsBullets(t *testing.T) {
	got := joinErrs("prefix", []error{
		errors.New("first thing broke"),
		errors.New("second thing broke"),
	})
	require.Error(t, got)
	msg := got.Error()
	assert.True(t, strings.HasPrefix(msg, "prefix (2 errors):"), "header should lead: %s", msg)
	assert.Contains(t, msg, "\n  - first thing broke")
	assert.Contains(t, msg, "\n  - second thing broke")
}

// TestDistinctTenantRoles_DedupesAndSorts pins the contract: the result
// is a stable set (sorted, no duplicates) so the plugin's invocation of
// runFromDDL creates roles in a deterministic order — generated output
// must be byte-stable across rebuilds.
func TestDistinctTenantRoles_DedupesAndSorts(t *testing.T) {
	plans := []*ResourcePlan{
		{Tenancy: &TenancyPlan{RuntimeRole: "app-tenant"}},
		{Tenancy: &TenancyPlan{RuntimeRole: "zeta-tenant"}},
		{Tenancy: &TenancyPlan{RuntimeRole: "app-tenant"}}, // dup — same DB
		{Tenancy: nil},                           // no tenancy — skipped
		{Tenancy: &TenancyPlan{RuntimeRole: ""}}, // empty — skipped
	}
	got := distinctTenantRoles(plans)
	assert.Equal(t, []string{"app-tenant", "zeta-tenant"}, got)
}

func TestDistinctTenantRoles_EmptyInput(t *testing.T) {
	assert.Nil(t, distinctTenantRoles(nil))
	assert.Nil(t, distinctTenantRoles([]*ResourcePlan{}))
}

// TestDetectDuplicateTables_SamePackage pins rejection when two
// messages in the same Go storage directory resolve to the same
// table name. The error lists both proto source locations so the
// author knows where to change the annotation.
func TestDetectDuplicateTables_SamePackage(t *testing.T) {
	pending := []pendingEmit{
		{source: "a.proto:10:1: Alpha", plan: &ResourcePlan{GoDir: "storage", TableName: "things"}},
		{source: "b.proto:20:1: Beta", plan: &ResourcePlan{GoDir: "storage", TableName: "things"}},
	}
	errs := detectDuplicateTables(pending)
	require.Len(t, errs, 1)
	msg := errs[0].Error()
	assert.Contains(t, msg, `"things"`)
	assert.Contains(t, msg, "a.proto:10:1")
	assert.Contains(t, msg, "b.proto:20:1")
}

// TestDetectDuplicateTables_DifferentPackages pins that a table-name
// reuse across different Go storage directories is NOT a collision.
// Different proto packages target different generated trees and
// usually different databases.
func TestDetectDuplicateTables_DifferentPackages(t *testing.T) {
	pending := []pendingEmit{
		{source: "a.proto", plan: &ResourcePlan{GoDir: "pkg_a/storage", TableName: "things"}},
		{source: "b.proto", plan: &ResourcePlan{GoDir: "pkg_b/storage", TableName: "things"}},
	}
	errs := detectDuplicateTables(pending)
	assert.Empty(t, errs)
}

// TestDetectDuplicateTables_NoCollision confirms the common-case
// two-distinct-tables path stays clean.
func TestDetectDuplicateTables_NoCollision(t *testing.T) {
	pending := []pendingEmit{
		{source: "a.proto", plan: &ResourcePlan{GoDir: "storage", TableName: "alphas"}},
		{source: "b.proto", plan: &ResourcePlan{GoDir: "storage", TableName: "betas"}},
	}
	errs := detectDuplicateTables(pending)
	assert.Empty(t, errs)
}

// TestDetectDuplicateTables_ThreeWayDupe pins that a three-message
// collision produces two errors (second + third colliding with
// first), not just one.
func TestDetectDuplicateTables_ThreeWayDupe(t *testing.T) {
	pending := []pendingEmit{
		{source: "a.proto", plan: &ResourcePlan{GoDir: "storage", TableName: "things"}},
		{source: "b.proto", plan: &ResourcePlan{GoDir: "storage", TableName: "things"}},
		{source: "c.proto", plan: &ResourcePlan{GoDir: "storage", TableName: "things"}},
	}
	errs := detectDuplicateTables(pending)
	assert.Len(t, errs, 2)
}
