-- Distinguish validation/claim failures from attempts that submitted SQL to PostgreSQL.
ALTER TABLE migration_job_attempts
  ADD COLUMN IF NOT EXISTS sql_started BOOLEAN NOT NULL DEFAULT FALSE;
