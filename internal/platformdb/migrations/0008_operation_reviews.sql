-- Rollback and forward are new database changes and require new approvals.
CREATE TABLE IF NOT EXISTS migration_operation_reviews (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  database_id BIGINT NOT NULL REFERENCES database_connections(id) ON DELETE RESTRICT,
  migration_id BIGINT NOT NULL REFERENCES migration_definitions(id) ON DELETE RESTRICT,
  revision BIGINT NOT NULL,
  operation TEXT NOT NULL CHECK (operation IN ('rollback','forward')),
  source_execution_id TEXT NOT NULL,
  actor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  up_sql TEXT NOT NULL,
  down_sql TEXT NOT NULL,
  workflow_status TEXT NOT NULL DEFAULT 'in_review'
    CHECK (workflow_status IN ('in_review','approved','rejected','queued','executed','failed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  job_id TEXT UNIQUE REFERENCES migration_jobs(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS migration_operation_reviews_migration_idx
  ON migration_operation_reviews(migration_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS migration_operation_reviews_active_unique
  ON migration_operation_reviews(migration_id,operation,source_execution_id)
  WHERE workflow_status IN ('in_review','approved','queued');

CREATE TABLE IF NOT EXISTS migration_operation_review_groups (
  review_id BIGINT NOT NULL REFERENCES migration_operation_reviews(id) ON DELETE RESTRICT,
  group_id BIGINT NOT NULL REFERENCES app_groups(id) ON DELETE RESTRICT,
  leader_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  access_level TEXT NOT NULL CHECK (access_level IN ('VIEW_ONLY','VIEW_EDIT')),
  PRIMARY KEY(review_id, group_id)
);
CREATE TABLE IF NOT EXISTS migration_operation_approvals (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  review_id BIGINT NOT NULL REFERENCES migration_operation_reviews(id) ON DELETE RESTRICT,
  group_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  decision TEXT NOT NULL CHECK (decision IN ('approved','rejected')),
  decided_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(review_id,group_id),
  FOREIGN KEY(review_id,group_id) REFERENCES migration_operation_review_groups(review_id,group_id) ON DELETE RESTRICT
);
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS operation_review_id BIGINT
  REFERENCES migration_operation_reviews(id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX IF NOT EXISTS migration_jobs_operation_review_unique
  ON migration_jobs(operation_review_id) WHERE operation_review_id IS NOT NULL;

CREATE OR REPLACE FUNCTION guard_operation_review() RETURNS trigger AS $$
BEGIN
  IF TG_OP='DELETE' THEN RAISE EXCEPTION 'operation reviews are append-only'; END IF;
  IF NEW.database_id IS DISTINCT FROM OLD.database_id
     OR NEW.migration_id IS DISTINCT FROM OLD.migration_id
     OR NEW.revision IS DISTINCT FROM OLD.revision
     OR NEW.operation IS DISTINCT FROM OLD.operation
     OR NEW.source_execution_id IS DISTINCT FROM OLD.source_execution_id
     OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
     OR NEW.up_sql IS DISTINCT FROM OLD.up_sql
     OR NEW.down_sql IS DISTINCT FROM OLD.down_sql
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.job_id IS NOT NULL AND NEW.job_id IS DISTINCT FROM OLD.job_id) THEN
    RAISE EXCEPTION 'operation review content is immutable';
  END IF;
  IF (OLD.workflow_status='in_review' AND NEW.workflow_status NOT IN ('in_review','approved','rejected'))
     OR (OLD.workflow_status='approved' AND NEW.workflow_status NOT IN ('approved','queued','executed','failed'))
     OR (OLD.workflow_status='queued' AND NEW.workflow_status NOT IN ('queued','executed','failed'))
     OR (OLD.workflow_status IN ('rejected','executed','failed') AND NEW.workflow_status<>OLD.workflow_status) THEN
    RAISE EXCEPTION 'invalid operation review transition';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS migration_operation_review_guard ON migration_operation_reviews;
CREATE TRIGGER migration_operation_review_guard BEFORE UPDATE OR DELETE ON migration_operation_reviews
  FOR EACH ROW EXECUTE FUNCTION guard_operation_review();

CREATE OR REPLACE FUNCTION reject_review_record_mutation() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'review records are append-only'; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS operation_review_groups_immutable ON migration_operation_review_groups;
CREATE TRIGGER operation_review_groups_immutable BEFORE UPDATE OR DELETE ON migration_operation_review_groups
  FOR EACH ROW EXECUTE FUNCTION reject_review_record_mutation();
DROP TRIGGER IF EXISTS operation_approvals_immutable ON migration_operation_approvals;
CREATE TRIGGER operation_approvals_immutable BEFORE UPDATE OR DELETE ON migration_operation_approvals
  FOR EACH ROW EXECUTE FUNCTION reject_review_record_mutation();
