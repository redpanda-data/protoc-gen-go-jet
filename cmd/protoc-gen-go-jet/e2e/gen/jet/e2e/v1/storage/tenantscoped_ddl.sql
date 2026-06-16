-- Code generated from jet/e2e/v1/tenancy.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.TenantScoped
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

CREATE TABLE tenant_scoped (
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name)
);

CREATE INDEX idx_tenant_scoped_tenant_id_created_at ON tenant_scoped (tenant_id, created_at DESC);
CREATE UNIQUE INDEX uniq_tenant_scoped_tenant_name ON tenant_scoped (tenant_id, name);
CREATE INDEX idx_tenant_scoped_enabled_status ON tenant_scoped (tenant_id, status) WHERE enabled = true;

ALTER TABLE tenant_scoped ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_scoped
    TO "e2e-tenant"
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

COMMENT ON TABLE tenant_scoped IS $$TenantScoped — composite PK (tenant_id, name), indexes with unique + partial + mixed order, RLS on `e2e-tenant`.$$;
COMMENT ON COLUMN tenant_scoped.status IS $$Plain orderable text column.$$;
