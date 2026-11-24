-- Durable, append-only execution identities; domain approval and immutable revision
-- verification MUST happen in the API and again through worker.Validator.
CREATE TABLE IF NOT EXISTS migration_jobs (
  id TEXT PRIMARY KEY,
  execution_id TEXT NOT NULL UNIQUE,
  database_id BIGINT NOT NULL REFERENCES database_connections(id),
  migration_id BIGINT NOT NULL,
  revision BIGINT NOT NULL CHECK (revision > 0),
  actor_id BIGINT NOT NULL REFERENCES users(id),
  operation TEXT NOT NULL CHECK (operation IN ('apply','forward','rollback')),
  mode TEXT NOT NULL CHECK (mode IN ('normal','batch')),
  sql_text TEXT NOT NULL,
  reverse_sql TEXT,
  batch_table TEXT,
  batch_size INTEGER CHECK (batch_size BETWEEN 1 AND 100000),
  source_execution_id TEXT,
  traceparent TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
    ('pending','running','retrying','success','failed','partial','partially_reverted','uncertain')),
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 4 CHECK (max_attempts BETWEEN 1 AND 20),
  available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  heartbeat_at TIMESTAMPTZ,
  batches_done BIGINT NOT NULL DEFAULT 0,
  rows_done BIGINT NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at TIMESTAMPTZ,
  CHECK ((mode = 'batch' AND batch_table IS NOT NULL AND batch_size IS NOT NULL)
      OR (mode = 'normal' AND batch_table IS NULL AND batch_size IS NULL)),
  CHECK (operation != 'rollback' OR source_execution_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS migration_jobs_ready_idx ON migration_jobs (available_at,created_at)
 WHERE status IN ('pending','retrying','partial','partially_reverted');
CREATE INDEX IF NOT EXISTS migration_jobs_db_status_idx ON migration_jobs (database_id,status);
CREATE TABLE IF NOT EXISTS migration_job_attempts (
  job_id TEXT NOT NULL REFERENCES migration_jobs(id),
  attempt INTEGER NOT NULL,
  started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  ended_at TIMESTAMPTZ,
  outcome TEXT,
  error_text TEXT,
  -- Per-attempt immutable execution context: failed SQL must remain auditable
  -- even when the target transaction is rolled back (QnA 2, answer 4/9).
  execution_id TEXT NOT NULL,
  migration_id BIGINT NOT NULL,
  revision BIGINT NOT NULL,
  actor_id BIGINT NOT NULL,
  operation TEXT NOT NULL,
  sql_text TEXT NOT NULL,
  PRIMARY KEY(job_id,attempt)
);
CREATE TABLE IF NOT EXISTS migration_worker_history (
  execution_id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES migration_jobs(id),
  database_id BIGINT NOT NULL,
  migration_id BIGINT NOT NULL,
  revision BIGINT NOT NULL,
  actor_id BIGINT NOT NULL,
  operation TEXT NOT NULL,
  source_execution_id TEXT,
  sql_text TEXT NOT NULL,
  committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
