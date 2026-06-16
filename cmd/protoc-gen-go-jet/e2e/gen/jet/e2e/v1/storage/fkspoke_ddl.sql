-- Code generated from jet/e2e/v1/foreign_keys.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.FKSpoke
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

CREATE TABLE fk_spoke (
    id      TEXT NOT NULL DEFAULT '',
    hub_id  TEXT NOT NULL DEFAULT '',
    note    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (id),
    CONSTRAINT fk_spoke_hub_id_fkey FOREIGN KEY (hub_id) REFERENCES fk_hub (id) ON DELETE CASCADE ON UPDATE NO ACTION
);

CREATE INDEX idx_fk_spoke_hub_id_fk ON fk_spoke (hub_id);

COMMENT ON TABLE fk_spoke IS $$FKSpoke — references FKHub with ON DELETE CASCADE to exercise a non-default action. Spokes disappear when their hub is deleted.$$;
COMMENT ON COLUMN fk_spoke.hub_id IS $$Single-column FK to FKHub.id. Plain path: no tenancy on either side. Backing btree index auto-emitted (hub_id is not covered by another declared index).$$;
