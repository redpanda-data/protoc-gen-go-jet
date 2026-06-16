package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// fkReferenceRe extracts the target table name from an inline FK
// constraint — `REFERENCES <table> (...)` — or the ALTER TABLE form
// kept around only so legacy callers outside this repo don't break
// if they handed us a pre-refactor DDL blob. Match is case-sensitive
// because unquoted PG identifiers fold to lowercase and the plugin
// only ever emits lowercase table names (validateSQLIdent enforces
// snake_case).
var fkReferenceRe = regexp.MustCompile(`REFERENCES\s+([a-z_][a-z0-9_]*)\s*\(`)

// createTableRe picks out the CREATE TABLE <name> header so the
// topo sorter knows which tables a given ddl.sql owns. A single
// plugin-emitted file carries exactly one CREATE TABLE today, but
// the regex tolerates files with several because custom_sql could
// legally add auxiliary tables via IF NOT EXISTS clauses.
var createTableRe = regexp.MustCompile(`(?mi)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s*\(`)

// topoSortDDL orders ddl.sql file contents so every file is applied
// after the files defining any of its FK targets. Ordering inputs:
//
//   - Nodes: every `CREATE TABLE <name> (` header found across the
//     input files. A file with two CREATE TABLEs contributes two
//     nodes that both point at the same file index.
//   - Edges: for every `REFERENCES <table> (` match, an edge from
//     the referring file to the file that owns <table>. Edges whose
//     target isn't created by any input file are ignored — those
//     reference an externally-managed table and the caller owns
//     ensuring it exists.
//
// Ties broken by input order so re-runs against an unchanged file
// set produce identical output. Returns the input files permuted
// to a valid apply order, or an error if the FK graph is cyclic
// (never possible from the plugin since the IDL rejects asymmetric
// tenancy and pg_schema_diff rejects cycles on apply — the check
// is defensive).
func topoSortDDL(files []FileContent) ([]FileContent, error) {
	// Build table -> owning-file-index map.
	tableOwner := map[string]int{}
	for i, f := range files {
		for _, m := range createTableRe.FindAllStringSubmatch(f.Content, -1) {
			tableOwner[m[1]] = i
		}
	}

	// Build adjacency + in-degree over file indices.
	n := len(files)
	adj := make([][]int, n)
	inDeg := make([]int, n)
	for i, f := range files {
		seen := map[int]bool{}
		for _, m := range fkReferenceRe.FindAllStringSubmatch(f.Content, -1) {
			target := m[1]
			owner, ok := tableOwner[target]
			if !ok {
				continue // external table
			}
			if owner == i {
				continue // self-reference — fine, not a DAG edge
			}
			if seen[owner] {
				continue // dedupe parallel edges from multi-column FKs
			}
			seen[owner] = true
			adj[owner] = append(adj[owner], i)
			inDeg[i]++
		}
	}

	// Kahn's algorithm with input-order tiebreak.
	ready := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if inDeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Ints(ready) // stable by input index
	out := make([]FileContent, 0, n)
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		out = append(out, files[i])
		for _, j := range adj[i] {
			inDeg[j]--
			if inDeg[j] == 0 {
				ready = append(ready, j)
			}
		}
		sort.Ints(ready)
	}

	if len(out) != n {
		var leftovers []string
		for i := 0; i < n; i++ {
			if inDeg[i] > 0 {
				leftovers = append(leftovers, files[i].Path)
			}
		}
		return nil, fmt.Errorf("foreign-key cycle among: %s", strings.Join(leftovers, ", "))
	}
	return out, nil
}

// FileContent pairs a ddl.sql path with its content so topoSortDDL
// can return the permuted list without callers having to re-read.
type FileContent struct {
	Path    string
	Content string
}
