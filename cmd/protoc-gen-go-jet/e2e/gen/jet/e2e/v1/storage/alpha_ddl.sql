-- Code generated from jet/e2e/v1/multi_resource.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Alpha
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

CREATE TABLE alphas (
    id          TEXT NOT NULL DEFAULT '',
    label       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

COMMENT ON TABLE alphas IS $$Alpha + Beta share one proto file and one Go storage package.$$;
COMMENT ON COLUMN alphas.id IS $$IDENTIFIER here is the e2e proof point for the field_behavior auto-mirror: no explicit `immutable: true` on the column option; the plugin must infer it from IDENTIFIER (AIP-203 says the resource identifier is immutable through Update). If the auto-mirror regresses, `AlphaUpdateAll()` would start listing `id` in its column set and the test below would fire.$$;
