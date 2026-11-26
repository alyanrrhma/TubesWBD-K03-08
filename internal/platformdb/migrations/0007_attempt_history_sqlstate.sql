-- Preserve PostgreSQL error classification with each immutable worker attempt.
ALTER TABLE migration_job_attempts
    ADD COLUMN IF NOT EXISTS sqlstate TEXT;
