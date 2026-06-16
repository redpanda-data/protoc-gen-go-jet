// jet runner — invoked from protoc-gen-go-jet's plugin mode and from
// the `from-db` subcommand. Boots an ephemeral Postgres, applies
// ddl.sql files, and delegates codegen to pkg/pgstore/jetgen. One jet
// config, two callers, identical output style.
//
// Tenant role defaults to "app-tenant". Override via the TENANT_ROLE
// env var.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-jet/jet/v2/generator/template"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver

	pkgtestcontainers "github.com/redpanda-data/protoc-gen-go-jet/internal/testcontainers"
	"github.com/redpanda-data/protoc-gen-go-jet/pkg/pgstore/jetgen"
)

// defaultTenantRole is the RLS role assumed when none is supplied.
// Override per invocation via the TENANT_ROLE environment variable.
const defaultTenantRole = "app-tenant"

// runFromDDL applies every ddl.sql under each ddl_root to an ephemeral
// PG container, then runs jet. Roles needed to evaluate ddl.sql (RLS
// policy targets) are passed via tenantRoles — the plugin reads them
// off each resource's tenancy spec. `overrides` routes specific
// (table, column) pairs to a non-default Go type on the jet model
// struct field — used for e.g. TIMESTAMPTZ[] columns where stdlib +
// pq ship no default scanner and the plugin supplies its own.
func runFromDDL(outDir string, roots []string, tenantRoles []string, overrides map[string]map[string]template.Type) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var ddls []string
	for _, root := range roots {
		found, err := discoverDDL(root)
		if err != nil {
			return fmt.Errorf("discover ddl in %q: %w", root, err)
		}
		ddls = append(ddls, found...)
	}
	if len(ddls) == 0 {
		return fmt.Errorf("no ddl.sql files under roots %v", roots)
	}
	slog.Info("applying ddl", slog.Int("files", len(ddls)), slog.Any("roots", roots))

	pg, err := pkgtestcontainers.NewPostgres()
	if err != nil {
		return fmt.Errorf("start ephemeral pg: %w", err)
	}
	defer func() {
		if err := pg.Terminate(context.Background()); err != nil {
			slog.Warn("terminate ephemeral pg", slog.Any("err", err))
		}
	}()

	// RLS policies in ddl.sql reference a runtime role, so it must
	// exist before DDL runs — even though we never connect as that
	// role for codegen (jet introspects as superuser).
	roles := tenantRoles
	if len(roles) == 0 {
		roles = []string{defaultTenantRole}
	}
	if v := os.Getenv("TENANT_ROLE"); v != "" {
		roles = []string{v}
	}
	super, err := pgx.Connect(ctx, pg.ConnectionString())
	if err != nil {
		return fmt.Errorf("connect super: %w", err)
	}
	defer func() { _ = super.Close(ctx) }()

	for _, role := range roles {
		if _, err := super.Exec(ctx, fmt.Sprintf(`
			DO $$ BEGIN
				IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
					CREATE ROLE "%[1]s" LOGIN PASSWORD '%[1]s';
				END IF;
			END $$;
		`, role)); err != nil {
			return fmt.Errorf("create tenant role %q: %w", role, err)
		}
	}
	// Topo-sort by inline FK references before applying. A referring
	// table's file must run after its target's file; topoSortDDL
	// extracts the dependency graph by grepping REFERENCES clauses.
	files := make([]FileContent, 0, len(ddls))
	for _, path := range ddls {
		b, err := os.ReadFile(path) //nolint:gosec // ddl paths come from the plugin's own discovery, not external input
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		files = append(files, FileContent{Path: path, Content: string(b)})
	}
	ordered, err := topoSortDDL(files)
	if err != nil {
		return fmt.Errorf("topo-sort ddl files: %w", err)
	}
	for _, f := range ordered {
		if _, err := super.Exec(ctx, f.Content); err != nil {
			return fmt.Errorf("apply %s: %w", f.Path, err)
		}
	}

	return runFromDB(pg.ConnectionString(), outDir, overrides)
}

// runFromDB is the escape hatch: generate against an arbitrary DSN.
// Uses pkg/pgstore/jetgen so the output matches the in-plugin codegen.
// `overrides` is passed through as `jetgen.Config.ColumnOverrides`.
func runFromDB(dsn, outDir string, overrides map[string]map[string]template.Type) error {
	return jetgen.GenerateFromDB(context.Background(), dsn, outDir, jetgen.Config{
		ColumnOverrides: overrides,
	})
}

// discoverDDL returns ddl.sql paths sorted lexicographically. Stable
// order means stable codegen.
func discoverDDL(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "*_ddl.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
