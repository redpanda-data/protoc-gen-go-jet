-- Code generated from jet/e2e/override/v1/override.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.override.v1.CustomLayout
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

CREATE TABLE custom_layout (
    id     TEXT NOT NULL DEFAULT '',
    label  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (id)
);

COMMENT ON TABLE custom_layout IS $$CustomLayout lives at a hand-picked Go directory rather than the auto-derived one. Everything else in the plugin still works through its regular code paths.$$;
