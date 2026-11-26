ALTER TABLE migration_definitions
    DROP CONSTRAINT IF EXISTS migration_definitions_workflow_status_check;
ALTER TABLE migration_definitions
    ADD CONSTRAINT migration_definitions_workflow_status_check
    CHECK (workflow_status IN ('draft','in_review','approved','rejected','executed'));

ALTER TABLE migration_revisions
    DROP CONSTRAINT IF EXISTS migration_revisions_workflow_status_check;
ALTER TABLE migration_revisions
    ADD CONSTRAINT migration_revisions_workflow_status_check
    CHECK (workflow_status IN ('draft','in_review','approved','rejected','executed'));

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
       OR (OLD.workflow_status = 'in_review' AND NEW.workflow_status NOT IN ('in_review','approved','rejected'))
       OR (OLD.workflow_status = 'approved' AND NEW.workflow_status NOT IN ('approved','executed'))
       OR (OLD.workflow_status = 'rejected' AND NEW.workflow_status <> 'rejected')
       OR (OLD.workflow_status = 'executed' AND NEW.workflow_status <> 'executed') THEN
        RAISE EXCEPTION 'invalid migration revision workflow transition';
    END IF;
    IF (NEW.submitted_at IS NOT NULL AND NEW.workflow_status NOT IN ('in_review','approved','rejected','executed'))
       OR (NEW.workflow_status IN ('in_review','approved','rejected','executed') AND NEW.submitted_at IS NULL)
       OR (NEW.workflow_status = 'executed' AND (NEW.executed_at IS NULL OR NEW.execution_id IS NULL))
       OR (NEW.workflow_status <> 'executed' AND (NEW.executed_at IS NOT NULL OR NEW.execution_id IS NOT NULL)) THEN
        RAISE EXCEPTION 'migration revision workflow timestamps are inconsistent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
