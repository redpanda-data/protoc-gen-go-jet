-- Code generated from jet/e2e/v1/nullable.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Nullable
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

CREATE TABLE nullable (
    id          TEXT NOT NULL DEFAULT '',
    string_opt  TEXT,
    bool_opt    BOOLEAN,
    int32_opt   INTEGER,
    int64_opt   BIGINT,
    bytes_opt   BYTEA,
    kind_opt    TEXT,
    deleted_at  TIMESTAMPTZ,
    ttl_opt     BIGINT,
    PRIMARY KEY (id),
    CONSTRAINT nullable_kind_opt_check CHECK (kind_opt IN ('NULLABLE_KIND_UNSPECIFIED','NULLABLE_KIND_A','NULLABLE_KIND_B'))
);

COMMENT ON COLUMN nullable.string_opt IS $$Proto3-optional scalars — synthetic-oneof presence via HasX().$$;
COMMENT ON COLUMN nullable.kind_opt IS $$Proto3-optional enum — HasX() + pointer enum type on the proto side. allow_zero_enum so the UNSPECIFIED variant is storable on the happy path (otherwise the CHECK would reject the zero value if a caller set it explicitly).$$;
COMMENT ON COLUMN nullable.deleted_at IS $$Timestamp — presence via the message pointer, no `optional` needed. Mapper reads t := p.GetX(); t == nil is the NULL signal.$$;
COMMENT ON COLUMN nullable.ttl_opt IS $$Proto3-optional Duration — the Duration message is already nilable, but the column is nullable: true so the mapper emits *int64 instead of defaulting to zero.$$;
