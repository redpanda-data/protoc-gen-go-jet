-- Code generated from jet/e2e/v1/multi_resource.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Beta
--
-- Canonical SQL schema for this resource. Regenerated from proto on
-- every `buf generate`. This file is a reference document; it is
-- not applied to the database. Your migrations directory holds the
-- deploy mechanism — hand-authored forward-only SQL.
--
-- Authoring a migration: `protoc-gen-go-jet diff` scaffolds an LLM brief
-- with a structural DELTA and the concatenated TARGET. The patterns live
-- in .claude/skills/protoc-gen-go-jet/SKILL.md. `protoc-gen-go-jet check`
-- is the machine-verified guardrail at the end of the flow.

CREATE TABLE betas (
    id          TEXT NOT NULL DEFAULT '',
    weight      BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);
