-- Code generated from jet/e2e/v1/foreign_keys.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.FKProject
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

CREATE TABLE fk_project (
    tenant_id  TEXT NOT NULL,
    id         TEXT NOT NULL DEFAULT '',
    org_id     TEXT NOT NULL DEFAULT '',
    title      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_project_org_id_fkey FOREIGN KEY (tenant_id, org_id) REFERENCES fk_org (tenant_id, id) ON DELETE RESTRICT ON UPDATE NO ACTION
);

CREATE INDEX idx_fk_project_org_id_fk ON fk_project (org_id);

ALTER TABLE fk_project ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON fk_project
    TO "e2e-tenant"
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

COMMENT ON TABLE fk_project IS $$FKProject — tenant-scoped child. The FK on `org_id` auto-expands to `(tenant_id, org_id) REFERENCES fk_org (tenant_id, id)` under the plugin's tenant-composite rule, preventing cross-tenant references structurally. Default actions (RESTRICT / NO ACTION) exercise the unspecified-defaults code path.$$;
