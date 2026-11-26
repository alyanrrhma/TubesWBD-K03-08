-- Keep execution identity and submitted SQL immutable; completed attempts and
-- successful execution history are append-only audit records.
CREATE OR REPLACE FUNCTION guard_job_attempt_history() RETURNS trigger AS $$
BEGIN
  IF TG_OP='DELETE' THEN RAISE EXCEPTION 'job attempts are append-only'; END IF;
  IF NEW.job_id IS DISTINCT FROM OLD.job_id
     OR NEW.attempt IS DISTINCT FROM OLD.attempt
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
     OR NEW.migration_id IS DISTINCT FROM OLD.migration_id
     OR NEW.revision IS DISTINCT FROM OLD.revision
     OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
     OR NEW.operation IS DISTINCT FROM OLD.operation
     OR NEW.sql_text IS DISTINCT FROM OLD.sql_text
     OR NEW.started_at IS DISTINCT FROM OLD.started_at THEN
    RAISE EXCEPTION 'job attempt execution context is immutable';
  END IF;
  IF OLD.ended_at IS NOT NULL THEN
    RAISE EXCEPTION 'completed job attempts are immutable';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS migration_job_attempt_history_guard ON migration_job_attempts;
CREATE TRIGGER migration_job_attempt_history_guard
  BEFORE UPDATE OR DELETE ON migration_job_attempts
  FOR EACH ROW EXECUTE FUNCTION guard_job_attempt_history();

CREATE OR REPLACE FUNCTION reject_worker_history_mutation() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'worker history is append-only'; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS migration_worker_history_immutable ON migration_worker_history;
CREATE TRIGGER migration_worker_history_immutable
  BEFORE UPDATE OR DELETE ON migration_worker_history
  FOR EACH ROW EXECUTE FUNCTION reject_worker_history_mutation();
