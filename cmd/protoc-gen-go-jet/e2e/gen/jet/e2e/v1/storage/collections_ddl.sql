-- Code generated from jet/e2e/v1/collections.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Collections
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

CREATE TABLE collections (
    id          TEXT NOT NULL DEFAULT '',
    labels      TEXT[],
    tags        TEXT[],
    items       JSONB,
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    attributes  JSONB,
    flags       BOOLEAN[],
    ints32      INTEGER[],
    uints32     INTEGER[],
    ints64      BIGINT[],
    uints64     BIGINT[],
    floats      REAL[],
    doubles     DOUBLE PRECISION[],
    PRIMARY KEY (id)
);

COMMENT ON TABLE collections IS $$Collections exercises repeated string/enum/message, nested message, and map<string,string>.$$;
COMMENT ON COLUMN collections.labels IS $$repeated string -> TEXT[]. Empty (nil), single, many, with special chars, unicode.$$;
COMMENT ON COLUMN collections.tags IS $$repeated enum -> TEXT[] of enum names.$$;
COMMENT ON COLUMN collections.items IS $$repeated message -> JSONB array.$$;
COMMENT ON COLUMN collections.payload IS $$Nested message -> JSONB (protojson).$$;
COMMENT ON COLUMN collections.attributes IS $$map<string,string> -> JSONB object.$$;
COMMENT ON COLUMN collections.flags IS $$Native-array primitive repeated fields — pq wrappers for BOOLEAN[] / INTEGER[] / BIGINT[] / REAL[] / DOUBLE PRECISION[]. Unsigned variants reinterpret element-wise through two's complement at the mapper, same trick as bare uint32/uint64 scalars.$$;
