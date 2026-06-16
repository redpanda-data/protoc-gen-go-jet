-- Code generated from jet/e2e/v1/oneofs.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.ScalarOneof
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

CREATE TABLE scalar_oneofs (
    id            TEXT NOT NULL DEFAULT '',
    payload_kind  TEXT NOT NULL DEFAULT '',
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id),
    CONSTRAINT scalar_oneofs_payload_kind_valid CHECK (payload_kind IN ('', 'enabled', 'note', 'score32', 'score64', 'tally32', 'tally64', 'ratio', 'measure', 'blob', 'flavour', 'a'))
);

COMMENT ON TABLE scalar_oneofs IS $$ScalarOneof exercises scalar + enum variants inside a persisted oneof. Storage shape is unchanged — a `_kind TEXT` discriminator + JSONB value; the JSONB holds the scalar's native JSON encoding (string / number / bool / base64-bytes / enum name) rather than a wrapped message.$$;
