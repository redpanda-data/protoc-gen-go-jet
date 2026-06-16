//go:build integration

// Package e2e — shared helpers for the integration harness. The whole
// file is gated on the `integration` build tag so `go build` and
// `go test -short` stay Docker-free.
package e2e

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver for database/sql
	"github.com/stretchr/testify/require"

	alphastorage "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/jet/e2e/v1/storage"
	customstorage "github.com/redpanda-data/protoc-gen-go-jet/cmd/protoc-gen-go-jet/e2e/gen/override_layout"
	pkgtestcontainers "github.com/redpanda-data/protoc-gen-go-jet/internal/testcontainers"
)

// e2eTenantRole is the RLS role referenced by every `tenancy:` spec in
// the fixture protos. Must exist in the container before the tenancy
// ddl.sql files are applied.
const e2eTenantRole = "e2e-tenant"

// cleanupTimeout caps how long an aborted bootstrap may spend
// terminating its container. Short because we're already on a failure
// path and don't want to block the test runner.
const cleanupTimeout = 30 * time.Second

// bootstrap starts a Postgres testcontainer, creates the RLS role,
// applies every generated ddl.sql in the e2e tree, and returns a
// super-user pool plus a tenant-scoped pool. Cleanup is the caller's
// responsibility — call pg.Terminate when done.
func bootstrap(ctx context.Context) (*pkgtestcontainers.Postgres, *sql.DB, *sql.DB, error) {
	pg, err := pkgtestcontainers.NewPostgres()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start postgres: %w", err)
	}

	bail := func(err error) (*pkgtestcontainers.Postgres, *sql.DB, *sql.DB, error) {
		// Derive cleanup ctx from the caller's so a cancelled test
		// doesn't hang terminating the container, but still inherits
		// deadline semantics.
		tCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = pg.Terminate(tCtx)
		return nil, nil, nil, err
	}

	if err := createTenantRole(ctx, pg); err != nil {
		return bail(err)
	}
	if err := applyAllDDL(ctx, pg); err != nil {
		return bail(err)
	}
	if err := grantTenantPrivileges(ctx, pg); err != nil {
		return bail(err)
	}

	superDB, err := sql.Open("pgx", pg.ConnectionString())
	if err != nil {
		return bail(fmt.Errorf("open super pool: %w", err))
	}
	if err := superDB.PingContext(ctx); err != nil {
		_ = superDB.Close()
		return bail(fmt.Errorf("ping super pool: %w", err))
	}

	tenantDSN := tenantConnString(pg.ConnectionString())
	tenantDB, err := sql.Open("pgx", tenantDSN)
	if err != nil {
		_ = superDB.Close()
		return bail(fmt.Errorf("open tenant pool: %w", err))
	}
	if err := tenantDB.PingContext(ctx); err != nil {
		_ = tenantDB.Close()
		_ = superDB.Close()
		return bail(fmt.Errorf("ping tenant pool: %w", err))
	}

	return pg, superDB, tenantDB, nil
}

// createTenantRole installs the RLS role referenced by every tenancy
// policy in the e2e fixtures. Idempotent — safe to call against a
// container that already has the role.
func createTenantRole(ctx context.Context, pg *pkgtestcontainers.Postgres) error {
	conn, err := pgx.Connect(ctx, pg.ConnectionString())
	if err != nil {
		return fmt.Errorf("connect super: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
				CREATE ROLE %[2]s LOGIN PASSWORD '%[1]s';
			END IF;
		END $$;
		GRANT CONNECT ON DATABASE postgres TO %[2]s;
		GRANT USAGE ON SCHEMA public TO %[2]s;
	`, e2eTenantRole, pgIdent(e2eTenantRole))); err != nil {
		return fmt.Errorf("create tenant role: %w", err)
	}
	return nil
}

// applyAllDDL runs every embedded ddl.sql from the generated packages
// against the container as super user. The plugin's embedded DDL is
// the same text the drift check asserts against migrations — using it
// here pins that the generated SQL is Postgres-accepted end-to-end.
//
// Topo-sorts the DDL blobs by their inline FK REFERENCES before
// applying so a referring table's CREATE TABLE lands after its
// referent's. The sort logic is deliberately duplicated from
// cmd/protoc-gen-go-jet/apply_ddl.go (which lives in `package main`
// and is therefore unimportable) rather than threaded through a
// shared package — it's ~30 lines of regex and Kahn's algorithm,
// and extracting a shared module just for the harness would touch
// more code than it saves.
func applyAllDDL(ctx context.Context, pg *pkgtestcontainers.Postgres) error {
	conn, err := pgx.Connect(ctx, pg.ConnectionString())
	if err != nil {
		return fmt.Errorf("connect super: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	ordered, err := topoSortDDLBlobs(allEmbeddedDDL())
	if err != nil {
		return fmt.Errorf("topo-sort embedded ddl: %w", err)
	}
	for i, ddl := range ordered {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("apply ddl[%d]: %w", i, err)
		}
	}
	return nil
}

// fkReferencePattern and createTablePattern mirror the plugin's own
// regexes in cmd/protoc-gen-go-jet/apply_ddl.go. The duplication is
// the cost of the harness living outside `package main`; a drift
// between the two regexes would manifest as tests failing with
// FK-constraint errors, which is the same failure mode the unit
// tests already cover.
var (
	fkReferencePattern = regexp.MustCompile(`REFERENCES\s+([a-z_][a-z0-9_]*)\s*\(`)
	createTablePattern = regexp.MustCompile(`(?mi)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s*\(`)
)

// topoSortDDLBlobs orders raw DDL strings so every referrer comes
// after its referent. See topoSortDDL in the plugin for the
// mechanics — this variant takes plain strings because the harness
// consumes `go:embed`'d DDL blobs rather than on-disk files.
func topoSortDDLBlobs(blobs []string) ([]string, error) {
	tableOwner := map[string]int{}
	for i, b := range blobs {
		for _, m := range createTablePattern.FindAllStringSubmatch(b, -1) {
			tableOwner[m[1]] = i
		}
	}
	n := len(blobs)
	adj := make([][]int, n)
	inDeg := make([]int, n)
	for i, b := range blobs {
		seen := map[int]bool{}
		for _, m := range fkReferencePattern.FindAllStringSubmatch(b, -1) {
			owner, ok := tableOwner[m[1]]
			if !ok || owner == i || seen[owner] {
				continue
			}
			seen[owner] = true
			adj[owner] = append(adj[owner], i)
			inDeg[i]++
		}
	}
	ready := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if inDeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Ints(ready)
	out := make([]string, 0, n)
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		out = append(out, blobs[i])
		for _, j := range adj[i] {
			inDeg[j]--
			if inDeg[j] == 0 {
				ready = append(ready, j)
			}
		}
		sort.Ints(ready)
	}
	if len(out) != n {
		return nil, errors.New("foreign-key cycle detected among embedded DDL blobs")
	}
	return out, nil
}

// allEmbeddedDDL returns every `<Prefix>DDL` embedded string the plugin
// emitted. applyAllDDL topo-sorts this list before applying, so the
// order here is purely documentary — group by category, not by FK
// dependency.
func allEmbeddedDDL() []string {
	return []string{
		alphastorage.ScalarsDDL,
		alphastorage.TimesDDL,
		alphastorage.CollectionsDDL,
		alphastorage.ColumnOptsDDL,
		alphastorage.RequiredOneofDDL,
		alphastorage.OptionalOneofDDL,
		alphastorage.ScalarOneofDDL,
		alphastorage.ClusterLikeDDL,
		alphastorage.WktDDL,
		alphastorage.NullableDDL,
		alphastorage.MapsDDL,
		alphastorage.AlphaDDL,
		alphastorage.BetaDDL,
		alphastorage.TenantScopedDDL,
		alphastorage.UserScopedDDL,
		alphastorage.FKHubDDL,
		alphastorage.FKSpokeDDL,
		alphastorage.FKOrgDDL,
		alphastorage.FKProjectDDL,
		customstorage.CustomLayoutDDL,
	}
}

// grantTenantPrivileges grants the tenant role CRUD on every table
// created above. DefaultPrivileges wouldn't help because the tables
// already exist by the time we grant.
func grantTenantPrivileges(ctx context.Context, pg *pkgtestcontainers.Postgres) error {
	conn, err := pgx.Connect(ctx, pg.ConnectionString())
	if err != nil {
		return fmt.Errorf("connect super: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, fmt.Sprintf(
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`,
		pgIdent(e2eTenantRole),
	))
	return err
}

// tenantConnString rewrites the super-user DSN to authenticate as the
// tenant role. Password matches the role name for the testcontainer —
// production wiring does this via SessionRunner + SET LOCAL
// app.tenant_id; here we open a dedicated pool because the e2e
// harness is intentionally not coupled to pkg/pgstore.
func tenantConnString(superDSN string) string {
	at := -1
	for i := 0; i < len(superDSN); i++ {
		if superDSN[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return superDSN
	}
	return fmt.Sprintf("postgresql://%s:%s%s", e2eTenantRole, e2eTenantRole, superDSN[at:])
}

// pgIdent double-quotes an identifier with any embedded quotes escaped.
// Needed because the role name contains a hyphen.
func pgIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}

// uniqueID returns a random 16-hex-char ID so parallel tests can mint
// non-colliding primary keys.
func uniqueID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
