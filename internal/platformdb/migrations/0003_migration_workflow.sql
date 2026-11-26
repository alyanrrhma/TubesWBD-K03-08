-- Migration definitions and append-only revision content for Milestone 2.
ALTER TABLE app_groups
    ADD COLUMN IF NOT EXISTS leader_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS migration_definitions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    description TEXT NOT NULL DEFAULT '',
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    workflow_status TEXT NOT NULL DEFAULT 'draft'
        CHECK (workflow_status IN ('draft','in_review','approved','executed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS migration_definitions_database_idx
    ON migration_definitions (database_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS migration_revisions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    definition_id BIGINT NOT NULL REFERENCES migration_definitions(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL CHECK (revision > 0),
    up_sql TEXT NOT NULL CHECK (length(btrim(up_sql)) > 0),
    down_sql TEXT,
    irreversible BOOLEAN NOT NULL DEFAULT FALSE,
    irreversible_reason TEXT,
    workflow_status TEXT NOT NULL DEFAULT 'draft'
        CHECK (workflow_status IN ('draft','in_review','approved','executed')),
    created_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    submitted_at TIMESTAMPTZ,
    executed_at TIMESTAMPTZ,
    execution_id TEXT UNIQUE,
    UNIQUE (definition_id, revision),
    CHECK ((workflow_status = 'draft' AND submitted_at IS NULL AND executed_at IS NULL AND execution_id IS NULL)
        OR (workflow_status IN ('in_review','approved') AND submitted_at IS NOT NULL AND executed_at IS NULL AND execution_id IS NULL)
        OR (workflow_status = 'executed' AND submitted_at IS NOT NULL AND executed_at IS NOT NULL AND execution_id IS NOT NULL)),
    CHECK ((irreversible AND length(btrim(COALESCE(irreversible_reason,''))) > 0)
        OR (NOT irreversible AND down_sql IS NOT NULL AND length(btrim(down_sql)) > 0))
);
CREATE INDEX IF NOT EXISTS migration_revisions_definition_idx
    ON migration_revisions (definition_id, revision DESC);

CREATE TABLE IF NOT EXISTS migration_approvals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision_id BIGINT NOT NULL REFERENCES migration_revisions(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES app_groups(id) ON DELETE RESTRICT,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    decision TEXT NOT NULL CHECK (decision IN ('approved','rejected')),
    decided_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (revision_id, group_id, user_id)
);
CREATE INDEX IF NOT EXISTS migration_approvals_revision_idx
    ON migration_approvals (revision_id, group_id);

-- Submitted or executed revision contents are frozen. Workflow metadata remains
-- mutable so approval and execution can advance the state without changing SQL.
CREATE OR REPLACE FUNCTION guard_migration_revision_content() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.submitted_at IS NOT NULL OR OLD.executed_at IS NOT NULL THEN
            RAISE EXCEPTION 'submitted or executed migration revisions cannot be deleted';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.submitted_at IS NOT NULL OR OLD.executed_at IS NOT NULL THEN
        IF NEW.definition_id IS DISTINCT FROM OLD.definition_id
           OR NEW.revision IS DISTINCT FROM OLD.revision
           OR NEW.up_sql IS DISTINCT FROM OLD.up_sql
           OR NEW.down_sql IS DISTINCT FROM OLD.down_sql
           OR NEW.irreversible IS DISTINCT FROM OLD.irreversible
           OR NEW.irreversible_reason IS DISTINCT FROM OLD.irreversible_reason
           OR NEW.created_by IS DISTINCT FROM OLD.created_by
           OR NEW.created_at IS DISTINCT FROM OLD.created_at
           OR NEW.submitted_at IS DISTINCT FROM OLD.submitted_at
           OR (OLD.executed_at IS NOT NULL AND NEW.executed_at IS DISTINCT FROM OLD.executed_at)
           OR (OLD.execution_id IS NOT NULL AND NEW.execution_id IS DISTINCT FROM OLD.execution_id) THEN
            RAISE EXCEPTION 'submitted or executed migration revision content is immutable';
        END IF;
    END IF;
    IF (OLD.workflow_status = 'draft' AND NEW.workflow_status NOT IN ('draft','in_review'))
       OR (OLD.workflow_status = 'in_review' AND NEW.workflow_status NOT IN ('in_review','approved'))
       OR (OLD.workflow_status = 'approved' AND NEW.workflow_status NOT IN ('approved','executed'))
       OR (OLD.workflow_status = 'executed' AND NEW.workflow_status <> 'executed') THEN
        RAISE EXCEPTION 'invalid migration revision workflow transition';
    END IF;
    IF (NEW.submitted_at IS NOT NULL AND NEW.workflow_status NOT IN ('in_review','approved','executed'))
       OR (NEW.workflow_status IN ('in_review','approved','executed') AND NEW.submitted_at IS NULL)
       OR (NEW.workflow_status = 'executed' AND (NEW.executed_at IS NULL OR NEW.execution_id IS NULL)) THEN
        RAISE EXCEPTION 'migration revision workflow timestamps are inconsistent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS migration_revision_content_guard ON migration_revisions;
CREATE TRIGGER migration_revision_content_guard
    BEFORE UPDATE OR DELETE ON migration_revisions
    FOR EACH ROW EXECUTE FUNCTION guard_migration_revision_content();

-- Removing a member who is the leader clears that assignment; the next review
-- submission will fail closed until an administrator appoints a new member.
CREATE OR REPLACE FUNCTION clear_group_leader_on_member_removal() RETURNS trigger AS $$
BEGIN
    UPDATE app_groups SET leader_user_id = NULL
      WHERE id = OLD.group_id AND leader_user_id = OLD.user_id;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS user_groups_clear_leader ON user_groups;
CREATE TRIGGER user_groups_clear_leader
    BEFORE DELETE ON user_groups
    FOR EACH ROW EXECUTE FUNCTION clear_group_leader_on_member_removal();
