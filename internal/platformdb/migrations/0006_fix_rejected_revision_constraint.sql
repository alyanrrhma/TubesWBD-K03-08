-- Remove the original workflow/timestamp constraint.
ALTER TABLE migration_revisions
    DROP CONSTRAINT IF EXISTS migration_revisions_check;

-- Restore the constraint with rejected as a valid terminal status.
ALTER TABLE migration_revisions
    ADD CONSTRAINT migration_revisions_check
    CHECK (
        (
            workflow_status = 'draft'
            AND submitted_at IS NULL
            AND executed_at IS NULL
            AND execution_id IS NULL
        )
        OR (
            workflow_status IN (
                'in_review', 'approved', 'rejected'
            )
            AND submitted_at IS NOT NULL
            AND executed_at IS NULL
            AND execution_id IS NULL
        )
        OR (
            workflow_status = 'executed'
            AND submitted_at IS NOT NULL
            AND executed_at IS NOT NULL
            AND execution_id IS NOT NULL
        )
    );