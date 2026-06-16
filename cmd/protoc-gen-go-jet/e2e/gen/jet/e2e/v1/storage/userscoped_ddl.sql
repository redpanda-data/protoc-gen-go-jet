-- Code generated from jet/e2e/v1/tenancy.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.UserScoped
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

CREATE TABLE user_scoped (
    tenant_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, user_id, name)
);

ALTER TABLE user_scoped ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_user_isolation ON user_scoped
    TO "e2e-tenant"
    USING      (tenant_id = current_setting('app.tenant_id', true)
                AND user_id = current_setting('app.user_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                AND user_id = current_setting('app.user_id', true));

COMMENT ON TABLE user_scoped IS $$UserScoped — tenant + user RLS. The synthesised `tenant_id` and `user_id` columns both flow through the RLS policy.$$;
