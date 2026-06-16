-- Code generated from jet/e2e/v1/maps.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Maps
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

CREATE TABLE maps (
    id                TEXT NOT NULL DEFAULT '',
    str_to_str        JSONB,
    str_to_int32      JSONB,
    str_to_int64      JSONB,
    str_to_bool       JSONB,
    str_to_bytes      JSONB,
    str_to_msg        JSONB,
    int32_to_str      JSONB,
    int64_to_str      JSONB,
    uint32_to_int32   JSONB,
    bool_to_str       JSONB,
    int32_to_msg      JSONB,
    nested_tree       JSONB,
    str_to_enum       JSONB,
    int32_to_enum     JSONB,
    str_to_timestamp  JSONB,
    str_to_duration   JSONB,
    sint32_to_str     JSONB,
    sint64_to_str     JSONB,
    uint64_to_str     JSONB,
    fixed32_to_str    JSONB,
    fixed64_to_str    JSONB,
    sfixed32_to_str   JSONB,
    sfixed64_to_str   JSONB,
    str_to_uint32     JSONB,
    str_to_uint64     JSONB,
    str_to_fixed32    JSONB,
    str_to_sfixed64   JSONB,
    PRIMARY KEY (id)
);

COMMENT ON COLUMN maps.str_to_str IS $$String-keyed fast path. Pins the byte-identical output of the stdlib-json map encoder.$$;
COMMENT ON COLUMN maps.str_to_int32 IS $$String key, every scalar value kind. Bytes goes through json's automatic base64 codec; bool / int* / string round-trip natively.$$;
COMMENT ON COLUMN maps.str_to_msg IS $$String key, message value — generic object-of-objects shape, the common case for structured attributes keyed by an identifier.$$;
COMMENT ON COLUMN maps.int32_to_str IS $$Integer and bool keys. Key stringification lives in the mapper; every integer kind uses base-10 ASCII, bool uses "true"/"false".$$;
COMMENT ON COLUMN maps.int32_to_msg IS $$Integer key, message value — combined coverage: non-string key path plus message-value protojson path in the same column.$$;
COMMENT ON COLUMN maps.nested_tree IS $$Deep nested fixture — the "map → object → map → object → repeated object" shape. Only the outer map lives in the DB; Level2/3/4 are opaque protojson inside the stored JSONB.$$;
COMMENT ON COLUMN maps.str_to_enum IS $$Enum-valued maps — each value stores its enum name, matching KindEnumAsText for bare enum scalars. Covers the string-keyed and non-string-keyed paths so the enum handling doesn't sneak in a regression for either.$$;
COMMENT ON COLUMN maps.str_to_timestamp IS $$Well-known message values — Timestamp / Duration ride the generic map<K, message> path through protojson, same as any other nested message. Cheap coverage that the mapper doesn't mishandle WKTs inside maps.$$;
COMMENT ON COLUMN maps.sint32_to_str IS $$Remaining proto3-legal integer key kinds. Every width + signedness combination exercises a distinct branch in the mapper's emitKeyToString / emitStringToKey codec. If the codec silently drops a kind, the round-trip fails with a parse error rather than a silent data-loss.$$;
COMMENT ON COLUMN maps.str_to_uint32 IS $$Remaining scalar value kinds the plugin persists. float/double are rejected at resolve time (not landed — see resolve.go's inferMapEncoding) so they're intentionally absent here; tests instead pin the rejection at the plugin-unit level.$$;
