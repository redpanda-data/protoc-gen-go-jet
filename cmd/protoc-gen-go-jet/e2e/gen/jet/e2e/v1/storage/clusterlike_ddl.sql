-- Code generated from jet/e2e/v1/cluster_like.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.ClusterLike
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

CREATE TABLE cluster_like (
    id                   TEXT NOT NULL DEFAULT '',
    name                 TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    state                TEXT NOT NULL DEFAULT 'STATE_UNSPECIFIED',
    state_description    JSONB NOT NULL DEFAULT '{}'::jsonb,
    zones                TEXT[],
    cloud_provider_tags  JSONB,
    spec                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    status               JSONB NOT NULL DEFAULT '{}'::jsonb,
    private_link         JSONB NOT NULL DEFAULT '{}'::jsonb,
    endpoints            JSONB,
    upgrade_window       BIGINT NOT NULL DEFAULT 0,
    connect_console      BOOLEAN,
    resource_version     BIGINT,
    cloud_kind           TEXT NOT NULL DEFAULT '',
    cloud                JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id),
    CONSTRAINT cluster_like_state_check CHECK (state IN ('STATE_UNSPECIFIED','STATE_CREATING','STATE_READY','STATE_DELETING','STATE_FAILED')),
    CONSTRAINT cluster_like_cloud_kind_valid CHECK (cloud_kind IN ('', 'aws', 'gcp'))
);

CREATE UNIQUE INDEX idx_cluster_like_name_unique ON cluster_like (name);

CREATE INDEX idx_cluster_like_cloud_provider_tags_env ON cluster_like ((cloud_provider_tags->>'env'));
CREATE INDEX idx_cluster_like_spec_installpackversion ON cluster_like ((spec->>'installPackVersion'));
CREATE INDEX idx_cluster_like_spec_storage_datadiskgib ON cluster_like ((spec->'storage'->>'dataDiskGib'));
CREATE INDEX idx_cluster_like_cloud_accountid ON cluster_like ((cloud->>'accountId'));
CREATE INDEX idx_cluster_like_cloud_projectid ON cluster_like ((cloud->>'projectId'));

CREATE INDEX idx_cluster_like_spec_gin ON cluster_like USING GIN (spec jsonb_path_ops);

COMMENT ON COLUMN cluster_like.name IS $$Unique shorthand — name is unique (no tenant scoping here; ClusterLike isn't tenant-annotated).$$;
COMMENT ON COLUMN cluster_like.created_at IS $$Simple scalar plus timestamps.$$;
COMMENT ON COLUMN cluster_like.state_description IS $$google.rpc.Status as a JSONB blob. This is the canonical "error detail" shape; needs protojson to survive the Any payloads that Status.details carries.$$;
COMMENT ON COLUMN cluster_like.zones IS $$Repeated string column.$$;
COMMENT ON COLUMN cluster_like.cloud_provider_tags IS $$map<string, string> — free-form tags. `env` gets a btree expression index so tag-by-env queries are fast.$$;
COMMENT ON COLUMN cluster_like.spec IS $$Spec carries both per-path btree indexes (fast equality on declared paths) and a GIN index over the full JSONB (covers `@>` containment queries on ANY sub-field). A caller running `spec @> '{"storage":{"tiered":true}}'` hits the GIN index without declaring `storage.tiered` as an indexed path.$$;
COMMENT ON COLUMN cluster_like.upgrade_window IS $$Durations.$$;
COMMENT ON COLUMN cluster_like.connect_console IS $$Proto3-optional scalars.$$;
COMMENT ON COLUMN cluster_like.cloud_kind IS $$Declared indexed paths on the oneof JSONB column. Paths are absolute inside the JSON value (no variant prefix) — the persisted JSON is `{"accountId": "..."}` for the AWS variant and `{"projectId": "..."}` for the GCP variant, so an index on `accountId` covers only rows whose active variant carries that field. Clients combine with `cloud_kind` for variant-specific queries: `cloud_kind = "aws" AND cloud.accountId = "..."`.$$;
COMMENT ON COLUMN cluster_like.cloud IS $$Declared indexed paths on the oneof JSONB column. Paths are absolute inside the JSON value (no variant prefix) — the persisted JSON is `{"accountId": "..."}` for the AWS variant and `{"projectId": "..."}` for the GCP variant, so an index on `accountId` covers only rows whose active variant carries that field. Clients combine with `cloud_kind` for variant-specific queries: `cloud_kind = "aws" AND cloud.accountId = "..."`.$$;

-- custom_sql — caller-supplied escape hatch; keep statements idempotent
CREATE INDEX IF NOT EXISTS idx_cluster_like_created_at_brin ON cluster_like USING BRIN (created_at);
