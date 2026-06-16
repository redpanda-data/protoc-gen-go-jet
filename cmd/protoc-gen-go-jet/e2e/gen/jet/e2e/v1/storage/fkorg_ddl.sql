-- Code generated from jet/e2e/v1/foreign_keys.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.FKOrg
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

CREATE TABLE fk_org (
    tenant_id  TEXT NOT NULL,
    id         TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

ALTER TABLE fk_org ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fk_org
    TO "e2e-tenant"
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

COMMENT ON TABLE fk_org IS $$FKOrg — tenant-scoped parent. PK (tenant_id, id) directly covers the (tenant_id, id) composite lookup, so FKProject's FK doesn't force a supplemental UNIQUE injection.$$;
