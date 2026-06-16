// Package e2e hosts the end-to-end harness for protoc-gen-go-jet.
//
// Fixture protos under proto/ exercise every proto3 kind and every
// option on the plugin's surface. buf generate drives both protoc-gen-go
// and the in-tree protoc-gen-go-jet plugin; output lands under gen/.
// The integration tests (//go:build integration) boot a Postgres
// testcontainer, apply the generated DDL, and round-trip every kind
// through the generated mapper + go-jet model.
//
// Regeneration is idempotent — commit the gen/ tree and CI asserts
// `go generate ./cmd/protoc-gen-go-jet/e2e/...` produces no drift.
package e2e

// Regenerate the fixtures. Builds the plugin, puts it (and protoc-gen-go
// from GOPATH/bin) on PATH, and runs buf from the repo root. Requires
// Docker (the plugin boots an ephemeral Postgres to drive go-jet's
// schema introspection) and protoc-gen-go installed
// (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`).
//
//go:generate bash -c "cd ../../.. && go build -o .build/bin/protoc-gen-go-jet ./cmd/protoc-gen-go-jet && PATH=$(pwd)/.build/bin:$(go env GOPATH)/bin:$PATH buf generate --template cmd/protoc-gen-go-jet/e2e/buf.gen.yaml"
