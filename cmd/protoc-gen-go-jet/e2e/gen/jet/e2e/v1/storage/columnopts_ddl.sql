-- Code generated from jet/e2e/v1/column_opts.proto by protoc-gen-go-jet. DO NOT EDIT.
--
-- Resource: jet.e2e.v1.ColumnOpts
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

CREATE TABLE column_opts (
    id             TEXT NOT NULL DEFAULT '',
    renamed_field  TEXT NOT NULL DEFAULT '',
    default_value  TEXT NOT NULL DEFAULT 'default-text',
    bounded_value  INTEGER NOT NULL DEFAULT 0,
    creator        TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'OPT_STATUS_UNSPECIFIED',
    new_label      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (id),
    CONSTRAINT column_opts_bounded_value_check CHECK (bounded_value >= 0 AND bounded_value <= 100),
    CONSTRAINT column_opts_status_check CHECK (status IN ('OPT_STATUS_UNSPECIFIED','OPT_STATUS_OPEN','OPT_STATUS_CLOSED'))
);

-- PLUGIN-RENAME directives — declared via `(storage.v1.column).rename_from`.
-- Author the next migration as `ALTER TABLE <t> RENAME COLUMN <old> TO <new>`
-- for each line below, then drop the annotation on the next release once
-- every environment has applied the rename.
-- PLUGIN-RENAME: column_opts old_label -> new_label

COMMENT ON TABLE column_opts IS $$ColumnOpts carries one column per knob so the integration test can verify each option's effect in isolation.$$;
COMMENT ON COLUMN column_opts.renamed_field IS $$`name:` override — proto field `original_field`, SQL column `renamed_field`.$$;
COMMENT ON COLUMN column_opts.default_value IS $$`default_expr` — the column has a custom default at the SQL level. When the caller doesn't set the field the INSERT picks up the default. `MODEL(row)` always writes the Go zero value though, so the integration test exercises the default via a direct SQL INSERT that omits the column.$$;
COMMENT ON COLUMN column_opts.bounded_value IS $$`check` — SQL CHECK that rejects bad values at the DB level.$$;
COMMENT ON COLUMN column_opts.creator IS $$`immutable` — excluded from the generated `<Prefix>UpdateAll()`. The integration test asserts the alias file's UPDATE list omits it.$$;
COMMENT ON COLUMN column_opts.status IS $$`orderable: true` + `allow_zero_enum: true` — the enum's zero value is persistable AND the column participates in pagination.$$;
COMMENT ON COLUMN column_opts.new_label IS $$`rename_from` — declares this column was renamed from a prior proto name. The plugin emits a PLUGIN-RENAME directive in the DDL so a migration author uses ALTER TABLE RENAME COLUMN instead of DROP+ADD. The runtime behaviour is identical to a plain column — the annotation only affects DDL.$$;
