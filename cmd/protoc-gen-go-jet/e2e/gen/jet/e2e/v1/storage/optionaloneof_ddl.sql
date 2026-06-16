-- Code generated from jet/e2e/v1/oneofs.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.OptionalOneof
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

CREATE TABLE optional_oneofs (
    id           TEXT NOT NULL DEFAULT '',
    choice_kind  TEXT NOT NULL DEFAULT '',
    choice       JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id),
    CONSTRAINT optional_oneofs_choice_kind_valid CHECK (choice_kind IN ('', 'a', 'b'))
);

COMMENT ON TABLE optional_oneofs IS $$OptionalOneof — absence maps to `_kind = ''` + JSONB `'{}'`.$$;
