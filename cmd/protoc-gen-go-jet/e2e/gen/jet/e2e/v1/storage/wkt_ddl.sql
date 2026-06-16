-- Code generated from jet/e2e/v1/wkt.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Wkt
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

CREATE TABLE wkt (
    id            TEXT NOT NULL DEFAULT '',
    string_wrap   TEXT,
    int32_wrap    INTEGER,
    int64_wrap    BIGINT,
    uint32_wrap   INTEGER,
    uint64_wrap   BIGINT,
    bool_wrap     BOOLEAN,
    bytes_wrap    BYTEA,
    float_wrap    REAL,
    double_wrap   DOUBLE PRECISION,
    struct_value  JSONB NOT NULL DEFAULT '{}'::jsonb,
    value_any     JSONB NOT NULL DEFAULT '{}'::jsonb,
    list_value    JSONB NOT NULL DEFAULT '{}'::jsonb,
    any_value     JSONB NOT NULL DEFAULT '{}'::jsonb,
    field_mask    JSONB NOT NULL DEFAULT '{}'::jsonb,
    empty_value   JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id)
);

COMMENT ON TABLE wkt IS $$Wkt has one column per supported well-known type. The resource doesn't model a real thing — it's a shape test for the mapper.$$;
COMMENT ON COLUMN wkt.string_wrap IS $$Wrappers — each serialises to its underlying JSON literal, not to the {"value": ...} shape. protojson handles the asymmetry.$$;
COMMENT ON COLUMN wkt.float_wrap IS $$Float / Double wrappers now route to nullable native REAL / DOUBLE PRECISION columns, same as the other primitive wrappers.$$;
COMMENT ON COLUMN wkt.struct_value IS $$Dynamic-JSON types. Struct is an object, Value is any JSON scalar or composite, ListValue is an array. All three round-trip via protojson's native-JSON form.$$;
COMMENT ON COLUMN wkt.any_value IS $$Any — carries a type_url discriminator + bytes payload. protojson serialises as {"@type": "...", "<field>": ...} for well-known payloads and {"@type": "...", "value": "<base64>"} otherwise.$$;
COMMENT ON COLUMN wkt.field_mask IS $$FieldMask — serialises as a string of comma-separated paths (canonical AIP-161 form). Stored as a JSONB string literal.$$;
COMMENT ON COLUMN wkt.empty_value IS $$Empty — has no fields; protojson serialises as {}. Round-trip is a presence-only check.$$;
