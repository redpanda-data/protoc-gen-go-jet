-- Code generated from jet/e2e/v1/scalars.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Scalars
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

CREATE TABLE scalars (
    id                 TEXT NOT NULL DEFAULT '',
    string_value       TEXT NOT NULL DEFAULT '',
    bool_value         BOOLEAN NOT NULL DEFAULT false,
    int32_value        INTEGER NOT NULL DEFAULT 0,
    sint32_value       INTEGER NOT NULL DEFAULT 0,
    sfixed32_value     INTEGER NOT NULL DEFAULT 0,
    uint32_value       INTEGER NOT NULL DEFAULT 0,
    fixed32_value      INTEGER NOT NULL DEFAULT 0,
    int64_value        BIGINT NOT NULL DEFAULT 0,
    sint64_value       BIGINT NOT NULL DEFAULT 0,
    sfixed64_value     BIGINT NOT NULL DEFAULT 0,
    float_value        REAL NOT NULL DEFAULT 0,
    double_value       DOUBLE PRECISION NOT NULL DEFAULT 0,
    uint64_value       BIGINT NOT NULL DEFAULT 0,
    fixed64_value      BIGINT NOT NULL DEFAULT 0,
    bytes_value        BYTEA NOT NULL DEFAULT ''::bytea,
    kind_value         TEXT NOT NULL DEFAULT 'SCALAR_KIND_UNSPECIFIED',
    kind_value_strict  TEXT NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT scalars_kind_value_check CHECK (kind_value IN ('SCALAR_KIND_UNSPECIFIED','SCALAR_KIND_ALPHA','SCALAR_KIND_BETA','SCALAR_KIND_GAMMA')),
    CONSTRAINT scalars_kind_value_strict_check CHECK (kind_value_strict IN ('SCALAR_KIND_ALPHA','SCALAR_KIND_BETA','SCALAR_KIND_GAMMA'))
);

COMMENT ON TABLE scalars IS $$Scalars covers every proto3 scalar kind listed in the plugin's field-kind inference table.$$;
COMMENT ON COLUMN scalars.id IS $$Row identifier. Stays immutable.$$;
COMMENT ON COLUMN scalars.string_value IS $$string -> TEXT.$$;
COMMENT ON COLUMN scalars.bool_value IS $$bool -> BOOLEAN.$$;
COMMENT ON COLUMN scalars.int32_value IS $$int32 -> INTEGER. Covers Zero, Min, Max, negative.$$;
COMMENT ON COLUMN scalars.sint32_value IS $$sint32 — zigzag-encoded, same SQL type.$$;
COMMENT ON COLUMN scalars.sfixed32_value IS $$sfixed32 — fixed-width 32-bit signed. INTEGER as well. This field is deliberately UN-annotated: it's the e2e proof point for the persist-by-default behaviour. If the round-trip test stops seeing an sfixed32 value come back, the plugin reverted to opt-in and we dropped a column silently.$$;
COMMENT ON COLUMN scalars.uint32_value IS $$uint32 -> INTEGER (int32 at the jet model). The mapper inserts explicit int32(...) / uint32(...) conversions either way.$$;
COMMENT ON COLUMN scalars.fixed32_value IS $$fixed32 — fixed-width 32-bit unsigned. Same INTEGER / int32 path.$$;
COMMENT ON COLUMN scalars.int64_value IS $$int64 -> BIGINT.$$;
COMMENT ON COLUMN scalars.sint64_value IS $$sint64 — zigzag 64-bit signed.$$;
COMMENT ON COLUMN scalars.sfixed64_value IS $$sfixed64 — fixed-width 64-bit signed.$$;
COMMENT ON COLUMN scalars.float_value IS $$float / double -> REAL / DOUBLE PRECISION. Round-trip is bit-exact at native SQL precision. Both are orderable — Float64Codec handles the page-token serialisation.$$;
COMMENT ON COLUMN scalars.uint64_value IS $$uint64 -> BIGINT (int64 at the jet model). Same uint/int cast pattern as uint32.$$;
COMMENT ON COLUMN scalars.fixed64_value IS $$fixed64 — fixed-width 64-bit unsigned.$$;
COMMENT ON COLUMN scalars.bytes_value IS $$bytes -> BYTEA. Empty, non-empty, arbitrary binary all round-trip.$$;
COMMENT ON COLUMN scalars.kind_value IS $$Enum -> TEXT with auto-CHECK. `allow_zero_enum: true` keeps the UNSPECIFIED variant in the CHECK so the integration test can round-trip every declared value.$$;
COMMENT ON COLUMN scalars.kind_value_strict IS $$Enum without `allow_zero_enum` — the auto-CHECK excludes UNSPECIFIED. Used to verify the CHECK rejects the zero value end-to-end.$$;
