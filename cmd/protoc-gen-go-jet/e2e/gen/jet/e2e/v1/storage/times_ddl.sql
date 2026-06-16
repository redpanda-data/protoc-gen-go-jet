-- Code generated from jet/e2e/v1/times.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.Times
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

CREATE TABLE times (
    id           TEXT NOT NULL DEFAULT '',
    event_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl          BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    checkpoints  TIMESTAMPTZ[],
    backoffs     BIGINT[],
    PRIMARY KEY (id)
);

COMMENT ON TABLE times IS $$Times exercises timestamp + duration persistence.$$;
COMMENT ON COLUMN times.event_at IS $$Timestamp -> TIMESTAMPTZ. Nanosecond precision round-trip.$$;
COMMENT ON COLUMN times.ttl IS $$Duration -> BIGINT nanoseconds. Zero, negative, large values.$$;
COMMENT ON COLUMN times.created_at IS $$Orderable timestamp — participates in List pagination + tie-breaker.$$;
COMMENT ON COLUMN times.checkpoints IS $$Repeated Timestamp lands in TIMESTAMPTZ[] via jettypes.TimestampArray — a named []time.Time type with Scan/Value methods the plugin threads through jetgen.ColumnOverrides (pq itself refuses to scan TIMESTAMPTZ[] into time.Time). Sub-μs nanoseconds truncate at the PG TIMESTAMPTZ floor; µs precision round-trips exact.$$;
COMMENT ON COLUMN times.backoffs IS $$Repeated Duration stays as BIGINT[] of nanoseconds — PG has no DURATION type, and Duration scalars already persist as BIGINT, so the repeated case is consistent with the scalar case.$$;
