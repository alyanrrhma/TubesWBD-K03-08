package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/telemetry"
	"github.com/jackc/pgx/v5/pgconn"
)

// Worker processes persisted jobs; multiple replicas can safely run concurrently.
// Each target database is fenced with a session-scoped PostgreSQL advisory lock.
type Worker struct {
	Platform          *sql.DB
	Targets           TargetResolver
	Validator         Validator
	Logger            *slog.Logger
	PollInterval      time.Duration
	DatabaseID        int64
	LeaseTimeout      time.Duration
	HeartbeatInterval time.Duration
	crashPoint        func(string)
}

func (w *Worker) leaseTimeout() time.Duration {
	if w.LeaseTimeout <= 0 {
		return 5 * time.Minute
	}
	return w.LeaseTimeout
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}
func (w *Worker) check() error {
	if w.Platform == nil || w.Targets == nil || w.Validator == nil {
		return errors.New("worker requires platform, target resolver and domain revalidator")
	}
	return nil
}
func (w *Worker) Run(ctx context.Context) error {
	if err := w.check(); err != nil {
		return err
	}
	poll := w.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger().Error("worker_poll_failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// RunOnce atomically claims one item; running jobs whose workers died can be
// reclaimed after a timeout. The database lock prevents concurrent writes.
func (w *Worker) RunOnce(ctx context.Context) error {
	if err := w.check(); err != nil {
		return err
	}
	j, err := w.claim(ctx)
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	return w.process(ctx, *j)
}
func (w *Worker) claim(ctx context.Context) (claimed *Job, retErr error) {
	tx, err := w.Platform.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var endClaim func(error)
	defer func() {
		if endClaim != nil {
			endClaim(retErr)
		}
	}()
	leaseMS := w.leaseTimeout().Milliseconds()
	if _, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status='uncertain',last_error='worker lease expired after maximum attempts; reconciliation required',finished_at=NOW()
		WHERE status='running' AND heartbeat_at<NOW()-($1 * INTERVAL '1 millisecond') AND attempt>=max_attempts`, leaseMS); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_job_attempts a SET ended_at=NOW(),outcome='uncertain',error_text='worker lease expired after maximum attempts; reconciliation required'
		FROM migration_jobs j WHERE a.job_id=j.id AND a.attempt=j.attempt AND a.ended_at IS NULL AND j.status='uncertain'
		AND j.last_error='worker lease expired after maximum attempts; reconciliation required'`); err != nil {
		return nil, err
	}
	// Block new work while a database is uncertain or partially migrated.
	row := tx.QueryRowContext(ctx, `SELECT id,execution_id,COALESCE(operation_review_id,0),database_id,migration_id,revision,actor_id,operation,mode,sql_text,
 COALESCE(reverse_sql,''),COALESCE(batch_table,''),COALESCE(batch_size,0),COALESCE(source_execution_id,''),
 traceparent,max_attempts,attempt,batches_done,rows_done,GREATEST(0,FLOOR(EXTRACT(EPOCH FROM (NOW()-j.created_at))*1000))::bigint FROM migration_jobs j
		WHERE ((j.status IN ('pending','retrying') AND j.available_at<=NOW())
		OR (j.status='running' AND j.heartbeat_at < NOW()-($1 * INTERVAL '1 millisecond')))
		AND j.attempt < j.max_attempts
		AND ($2=0 OR j.database_id=$2)
		AND NOT EXISTS(SELECT 1 FROM migration_jobs b WHERE b.database_id=j.database_id AND b.id<>j.id
 AND b.status IN ('running','partial','partially_reverted','uncertain'))
	ORDER BY j.created_at,j.id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`, leaseMS, w.DatabaseID)
	var j Job
	err = row.Scan(&j.ID, &j.ExecutionID, &j.OperationReviewID, &j.DatabaseID, &j.MigrationID, &j.Revision, &j.ActorID, &j.Operation, &j.Mode,
		&j.SQL, &j.ReverseSQL, &j.BatchTable, &j.BatchSize, &j.SourceExecutionID, &j.Traceparent, &j.MaxAttempts, &j.Attempt, &j.BatchesDone, &j.RowsDone, &j.QueueWaitMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ctx, endClaim = telemetry.Start(telemetry.FromHeader(ctx, j.Traceparent), "worker.queue.claim", map[string]any{"job_id": j.ID, "execution_id": j.ExecutionID, "database_id": j.DatabaseID, "queue_wait_ms": j.QueueWaitMS})
	j.Traceparent = telemetry.Traceparent(ctx)
	if j.Attempt > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_job_attempts
 SET ended_at=COALESCE(ended_at,NOW()),outcome=COALESCE(outcome,'worker_interrupted')
 WHERE job_id=$1 AND attempt=$2 AND ended_at IS NULL`, j.ID, j.Attempt); err != nil {
			return nil, err
		}
	}
	j.Attempt++
	if _, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status='running',attempt=$2,heartbeat_at=NOW() WHERE id=$1`, j.ID, j.Attempt); err != nil {
		return nil, err
	}
	// Record the SQL/revision/actor for every attempt, including SQL that later
	// rolls back. Migration history still tracks committed executions separately.
	if _, err = tx.ExecContext(ctx, `INSERT INTO migration_job_attempts
 (job_id,attempt,execution_id,migration_id,revision,actor_id,operation,sql_text)
 SELECT id,$2,execution_id,migration_id,revision,actor_id,operation,
 CASE WHEN operation='rollback' THEN reverse_sql ELSE sql_text END
 FROM migration_jobs WHERE id=$1 ON CONFLICT DO NOTHING`, j.ID, j.Attempt); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}
func (w *Worker) process(ctx context.Context, j Job) error {
	start := time.Now()
	ctx, endWorker := telemetry.Start(telemetry.FromHeader(ctx, j.Traceparent), "worker.migration", map[string]any{"execution_id": j.ExecutionID, "migration_id": j.MigrationID, "attempt": j.Attempt})
	var spanError error
	defer func() { endWorker(spanError) }()
	log := w.logger().With("job_id", j.ID, "execution_id", j.ExecutionID, "migration_id", j.MigrationID,
		"revision", j.Revision, "database", j.DatabaseID, "actor", j.ActorID, "attempt", j.Attempt, "operation", j.Operation,
		"trace_id", telemetry.TraceID(ctx), "span_id", telemetry.SpanID(ctx))
	log.Info("job_started", "status", "running", "sql_text", effectiveSQL(j))
	if err := j.Validate(); err != nil {
		return w.finish(ctx, j, err, false)
	}
	if err := w.Validator.Revalidate(ctx, j); err != nil {
		return w.finish(ctx, j, fmt.Errorf("revalidate: %w", err), false)
	}
	target, err := w.Targets.Get(j.DatabaseID)
	if err != nil {
		return w.finish(ctx, j, err, true)
	}
	// Pin one target connection for the *entire* job; a PostgreSQL session lock
	// survives commits between batches and is released if the worker crashes.
	conn, err := target.Conn(ctx)
	if err != nil {
		return w.finish(ctx, j, err, true)
	}
	defer conn.Close()
	lockKey := int64(31100000) + j.DatabaseID
	var locked bool
	lockCtx, endLock := telemetry.Start(ctx, "worker.database.lock.acquire", map[string]any{"database_id": j.DatabaseID, "job_id": j.ID})
	if err = conn.QueryRowContext(lockCtx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&locked); err != nil {
		endLock(err)
		return w.finish(ctx, j, err, true)
	}
	endLock(nil)
	if !locked {
		log.Info("lock_busy", "status", "retrying")
		return w.deferBusy(ctx, j)
	}
	defer func() {
		unlockCtx, endUnlock := telemetry.Start(context.WithoutCancel(ctx), "worker.database.lock.release", map[string]any{"database_id": j.DatabaseID, "job_id": j.ID})
		unlockCtx, cancelUnlock := context.WithTimeout(unlockCtx, 5*time.Second)
		defer cancelUnlock()
		_, unlockErr := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockKey)
		endUnlock(unlockErr)
	}()
	log.Info("lock_acquired", "status", "running")
	// A separate platform connection renews the lease during long SQL statements.
	// Stop and join the heartbeat before returning (including on panic-free error paths).
	runCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	stopHeartbeat := w.startHeartbeat(runCtx, j, log, cancelJob)
	defer stopHeartbeat()
	ctx = runCtx
	// No schema mutation is accepted until the target-side proof ledger exists.
	if err = w.ensureProof(ctx, conn); err != nil {
		return w.finish(ctx, j, err, false)
	}
	if err = w.Validator.Revalidate(ctx, j); err != nil {
		return w.finish(ctx, j, fmt.Errorf("locked revalidate: %w", err), false)
	}
	if j.Mode == Normal {
		err = w.normal(ctx, conn, j, log)
	} else if j.Operation == Rollback {
		err = w.reverseBatches(ctx, conn, j, log)
	} else {
		err = w.batches(ctx, conn, j, log)
	}
	if errors.Is(err, ErrUncertain) {
		spanError = err
		log.Error("commit_uncertain", "status", "uncertain", "duration", time.Since(start))
		return w.uncertain(ctx, j, err)
	}
	if errors.Is(err, ErrLostLease) {
		spanError = err
		return err
	}
	if err != nil {
		spanError = err
		// The target is authoritative: a platform counter might lag a committed batch.
		if syncErr := w.syncProgress(ctx, conn, j); syncErr != nil {
			log.Error("checkpoint_reconciliation_failed", "error", syncErr)
			return w.uncertain(ctx, j, fmt.Errorf("cannot reconcile committed checkpoints: %w", syncErr))
		}
		return w.finish(ctx, j, err, isTransient(err))
	}
	if err = w.syncProgress(ctx, conn, j); err != nil {
		return w.uncertain(ctx, j, fmt.Errorf("cannot reconcile successful execution: %w", err))
	}
	log.Info("job_succeeded", "status", "success", "duration", time.Since(start))
	return w.succeed(ctx, j)
}
func (w *Worker) ensureProof(ctx context.Context, c *sql.Conn) error {
	_, err := c.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS __dbmp_worker_proof (
 execution_id TEXT NOT NULL, batch_no BIGINT NOT NULL,
 source_execution_id TEXT, batch_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
 committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY(execution_id,batch_no))`)
	return err
}
func (w *Worker) proof(ctx context.Context, c *sql.Conn, exec string, batch int64) (bool, error) {
	var ok bool
	err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no=$2)`, exec, batch).Scan(&ok)
	return ok, err
}
func (w *Worker) normal(ctx context.Context, c *sql.Conn, j Job, log *slog.Logger) error {
	done, err := w.proof(ctx, c, j.ExecutionID, 0)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	statement := j.SQL
	if j.Operation == Rollback {
		statement = j.ReverseSQL
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	started := time.Now()
	w.crashAt("before_sql")
	log.Info("sql_started", "batch", 0, "query", statement, "parameters", []any{})
	if err = w.markSQLStarted(ctx, j); err != nil {
		return err
	}
	sqlCtx, endSQL := telemetry.Start(ctx, "db.migration.sql", map[string]any{"operation": j.Operation, "mode": j.Mode, "batch": 0, "query": statement})
	res, err := tx.ExecContext(sqlCtx, statement)
	endSQL(err)
	if err != nil {
		rollbackTransaction(ctx, tx, log, "migration", 0, err)
		log.Error("sql_failed", "error", err, "sqlstate", sqlState(err), "duration", time.Since(started))
		return err
	}
	w.crashAt("before_commit")
	rows, _ := res.RowsAffected()
	log.Info("sql_finished", "rows", rows, "duration", time.Since(started))
	if _, err = tx.ExecContext(ctx, `INSERT INTO __dbmp_worker_proof(execution_id,batch_no,source_execution_id) VALUES($1,0,$2)`, j.ExecutionID, nullable(j.SourceExecutionID)); err != nil {
		return err
	}
	log.Info("commit_started", "batch", 0)
	if err = commitTransaction(ctx, tx, log, "migration", 0); err != nil {
		return w.commitError(ctx, c, j.ExecutionID, 0, err)
	}
	w.crashAt("after_commit")
	log.Info("commit_succeeded", "batch", 0)
	return nil
}
func (w *Worker) commitError(ctx context.Context, c *sql.Conn, exec string, batch int64, commitErr error) error {
	// A lost acknowledgment must NEVER be blindly retried. If proof exists,
	// target committed; if the DB cannot be queried, the job becomes uncertain.
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ok, err := w.proof(probe, c, exec, batch)
	if err != nil {
		return fmt.Errorf("%w: commit=%v; probe=%v", ErrUncertain, commitErr, err)
	}
	if ok {
		return nil
	}
	return fmt.Errorf("commit failed (proof absent): %w", commitErr)
}
func (w *Worker) upperBound(ctx context.Context, c *sql.Conn, j Job) (int64, error) {
	// Identifiers must be allowlisted by domain validator; lexical validation
	// here prevents injected SQL in a separately constructed cursor query.
	if !identifier.MatchString(j.BatchTable) {
		return 0, ErrInvalidJob
	}
	var upper sql.NullInt64
	err := c.QueryRowContext(ctx, `SELECT MAX(id) FROM "`+j.BatchTable+`"`).Scan(&upper)
	return upper.Int64, err
}
func (w *Worker) resume(ctx context.Context, c *sql.Conn, j Job) (int64, int64, int64, error) {
	// Upper bound is snapshotted in proof row -1, persisted in the target DB.
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	var upper int64
	err = tx.QueryRowContext(ctx, `SELECT (batch_ids->>0)::bigint FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no=-1`, j.ExecutionID).Scan(&upper)
	if errors.Is(err, sql.ErrNoRows) {
		if !identifier.MatchString(j.BatchTable) {
			return 0, 0, 0, ErrInvalidJob
		}
		var maximum sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT MAX(id) FROM "`+j.BatchTable+`"`).Scan(&maximum)
		if err != nil {
			return 0, 0, 0, err
		}
		upper = maximum.Int64
		if _, err = tx.ExecContext(ctx, `INSERT INTO __dbmp_worker_proof(execution_id,batch_no,batch_ids) VALUES($1,-1,jsonb_build_array($2::bigint)) ON CONFLICT DO NOTHING`, j.ExecutionID, upper); err != nil {
			return 0, 0, 0, fmt.Errorf("persist batch upper bound: %w", err)
		}
	} else if err != nil {
		return 0, 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	rows, err := c.QueryContext(ctx, `SELECT batch_no,batch_ids FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no>=0 ORDER BY batch_no`, j.ExecutionID)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	var next, cursor int64
	for rows.Next() {
		var n int64
		var raw []byte
		if err = rows.Scan(&n, &raw); err != nil {
			return 0, 0, 0, err
		}
		var ids []int64
		if err = json.Unmarshal(raw, &ids); err != nil {
			return 0, 0, 0, err
		}
		if n != next {
			return 0, 0, 0, fmt.Errorf("non-contiguous checkpoint %d", n)
		}
		next++
		if len(ids) > 0 {
			cursor = ids[len(ids)-1]
		}
	}
	return upper, next, cursor, rows.Err()
}
func (w *Worker) batches(ctx context.Context, c *sql.Conn, j Job, log *slog.Logger) error {
	upper, next, cursor, err := w.resume(ctx, c, j)
	if err != nil {
		return err
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		// DB guard must remain alive even if the worker loses its queue connection.
		if err = w.heartbeat(ctx, j); err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		tx, e := c.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		scanQuery := `SELECT id FROM "` + j.BatchTable + `" WHERE id>$1::bigint AND id<=$2::bigint ORDER BY id LIMIT $3::integer`
		log.Info("sql_query_started", "query", scanQuery, "parameters", []any{cursor, upper, j.BatchSize})
		startedScan := time.Now()
		ids, e := selectIDs(ctx, tx, j.BatchTable, cursor, upper, j.BatchSize)
		log.Info("sql_query_finished", "query", scanQuery,
			"parameters", []any{cursor, upper, j.BatchSize}, "duration", time.Since(startedScan), "row_count", len(ids), "status", map[bool]string{true: "error", false: "success"}[e != nil], "error", e, "sqlstate", sqlState(e))
		if e != nil {
			rollbackBatch(ctx, tx, log, next, e)
			return e
		}
		if len(ids) == 0 {
			rollbackBatch(ctx, tx, log, next, nil)
			log.Info("batch_scan_finished", "batch", next, "selected_rows", 0, "status", "complete")
			return nil
		}
		started := time.Now()
		w.crashAt("before_sql")
		log.Info("sql_started", "batch", next, "selected_rows", len(ids), "query", j.SQL, "parameter_count", len(ids), "parameters_redacted", true)
		if e = w.markSQLStarted(ctx, j); e != nil {
			rollbackBatch(ctx, tx, log, next, e)
			return e
		}
		sqlCtx, endSQL := telemetry.Start(ctx, "db.migration.batch", map[string]any{"batch": next, "rows": len(ids), "query": j.SQL})
		res, e := tx.ExecContext(sqlCtx, j.SQL, ids)
		endSQL(e)
		if e != nil {
			rollbackBatch(ctx, tx, log, next, e)
			log.Error("batch_failed", "batch", next, "error", e, "sqlstate", sqlState(e), "duration", time.Since(started))
			return e
		}
		affected, e := res.RowsAffected()
		if e != nil {
			rollbackBatch(ctx, tx, log, next, e)
			return e
		}
		if affected != int64(len(ids)) {
			e = fmt.Errorf("batch SQL affected %d of %d selected IDs; unsafe checkpoint", affected, len(ids))
			rollbackBatch(ctx, tx, log, next, e)
			return e
		}
		w.crashAt("before_commit")
		raw, _ := json.Marshal(ids)
		_, e = tx.ExecContext(ctx, `INSERT INTO __dbmp_worker_proof(execution_id,batch_no,batch_ids) VALUES($1,$2,$3::jsonb)`, j.ExecutionID, next, string(raw))
		if e != nil {
			rollbackBatch(ctx, tx, log, next, e)
			return e
		}
		log.Info("commit_started", "batch", next, "rows", affected)
		if e = commitTransaction(ctx, tx, log, "batch", next); e != nil {
			if e = w.commitError(ctx, c, j.ExecutionID, next, e); e != nil {
				return e
			}
		}
		log.Info("batch_committed", "batch", next, "rows", affected, "duration", time.Since(started))
		w.crashAt("after_commit")
		next++
		cursor = ids[len(ids)-1]
		if err = w.syncProgress(ctx, c, j); err != nil {
			return err
		}
	}
}
func selectIDs(ctx context.Context, tx *sql.Tx, table string, cursor, upper int64, limit int) ([]int64, error) {
	if !identifier.MatchString(table) {
		return nil, ErrInvalidJob
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM "`+table+`" WHERE id>$1::bigint AND id<=$2::bigint ORDER BY id LIMIT $3::integer`, cursor, upper, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (w *Worker) reverseBatches(ctx context.Context, c *sql.Conn, j Job, log *slog.Logger) error {
	// Each compensation is an independent, durable transaction. Reverse order
	// is crucial when later batches depend on earlier changed rows.
	var upper int64
	if err := c.QueryRowContext(ctx, `SELECT COALESCE(MAX(batch_no),-1) FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no>=0`, j.SourceExecutionID).Scan(&upper); err != nil {
		return err
	}
	if upper < 0 {
		return errors.New("no committed source batches; refusing unsafe rollback")
	}
	for n := upper; n >= 0; n-- {
		var b struct {
			n   int64
			ids []int64
		}
		b.n = n
		var raw []byte
		if err := c.QueryRowContext(ctx, `SELECT batch_ids FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no=$2`, j.SourceExecutionID, n).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &b.ids); err != nil {
			return err
		}

		if err := w.heartbeat(ctx, j); err != nil {
			return err
		}
		done, e := w.proof(ctx, c, j.ExecutionID, b.n)
		if e != nil {
			return e
		}
		if done {
			continue
		}
		tx, e := c.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		log.Info("compensation_started", "batch", b.n, "selected_rows", len(b.ids), "query", j.ReverseSQL, "parameter_count", len(b.ids), "parameters_redacted", true)
		w.crashAt("rollback_before_sql")
		if e = w.markSQLStarted(ctx, j); e != nil {
			rollbackBatch(ctx, tx, log, b.n, e)
			return e
		}
		sqlCtx, endSQL := telemetry.Start(ctx, "db.migration.compensate", map[string]any{"batch": b.n, "rows": len(b.ids), "query": j.ReverseSQL})
		res, e := tx.ExecContext(sqlCtx, j.ReverseSQL, b.ids)
		endSQL(e)
		if e != nil {
			rollbackBatch(ctx, tx, log, b.n, e)
			log.Error("compensation_failed", "batch", b.n, "error", e, "sqlstate", sqlState(e))
			return e
		}
		affected, e := res.RowsAffected()
		if e != nil {
			rollbackBatch(ctx, tx, log, b.n, e)
			return e
		}
		if affected != int64(len(b.ids)) {
			e = fmt.Errorf("compensation affected %d of %d rows", affected, len(b.ids))
			rollbackBatch(ctx, tx, log, b.n, e)
			return e
		}
		w.crashAt("rollback_before_commit")
		raw, _ = json.Marshal(b.ids)
		_, e = tx.ExecContext(ctx, `INSERT INTO __dbmp_worker_proof(execution_id,batch_no,source_execution_id,batch_ids) VALUES($1,$2,$3,$4::jsonb)`, j.ExecutionID, b.n, j.SourceExecutionID, string(raw))
		if e != nil {
			rollbackBatch(ctx, tx, log, b.n, e)
			return e
		}
		log.Info("commit_started", "batch", b.n)
		if e = commitTransaction(ctx, tx, log, "compensation", b.n); e != nil {
			if e = w.commitError(ctx, c, j.ExecutionID, b.n, e); e != nil {
				return e
			}
		}
		log.Info("compensation_committed", "batch", b.n, "rows", affected)
		w.crashAt("rollback_after_commit")
		if err := w.syncProgress(ctx, c, j); err != nil {
			return err
		}
	}
	return nil
}

func rollbackBatch(ctx context.Context, tx *sql.Tx, log *slog.Logger, batch int64, cause error) {
	rollbackTransaction(ctx, tx, log, "batch", batch, cause)
}
func rollbackTransaction(ctx context.Context, tx *sql.Tx, log *slog.Logger, operation string, batch int64, cause error) {
	spanCtx, end := telemetry.Start(ctx, "db.transaction.rollback", map[string]any{"operation": operation, "batch": batch})
	err := tx.Rollback()
	end(err)
	log.Info("rollback_finished", "operation", operation, "batch", batch, "status", "rolled_back", "cause", cause, "rollback_error", err, "span_id", telemetry.SpanID(spanCtx))
}
func commitTransaction(ctx context.Context, tx *sql.Tx, log *slog.Logger, operation string, batch int64) error {
	spanCtx, end := telemetry.Start(ctx, "db.transaction.commit", map[string]any{"operation": operation, "batch": batch})
	err := tx.Commit()
	end(err)
	status := "committed"
	if err != nil {
		status = "failed"
	}
	log.Info("commit_finished", "operation", operation, "batch", batch, "status", status, "error", err, "span_id", telemetry.SpanID(spanCtx))
	return err
}
func (w *Worker) heartbeat(ctx context.Context, j Job) error {
	res, err := w.Platform.ExecContext(ctx, `UPDATE migration_jobs SET heartbeat_at=NOW() WHERE id=$1 AND attempt=$2 AND status='running'`, j.ID, j.Attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLostLease
	}
	return nil
}

func (w *Worker) crashAt(point string) {
	if w.crashPoint != nil {
		w.crashPoint(point)
	}
}

func (w *Worker) markSQLStarted(ctx context.Context, j Job) error {
	result, err := w.Platform.ExecContext(ctx, `UPDATE migration_job_attempts a SET sql_started=TRUE WHERE a.job_id=$1 AND a.attempt=$2 AND a.ended_at IS NULL
		AND EXISTS(SELECT 1 FROM migration_jobs j WHERE j.id=a.job_id AND j.attempt=a.attempt AND j.status='running')`, j.ID, j.Attempt)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLostLease
	}
	return nil
}

// syncProgress reconciles platform counters from target-side committed proofs.
// Re-running it after a crash cannot double-count committed batches or rows.
func (w *Worker) syncProgress(ctx context.Context, c *sql.Conn, j Job) error {
	var batches, rows int64
	err := c.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(jsonb_array_length(batch_ids)),0)
 FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no>=0`, j.ExecutionID).Scan(&batches, &rows)
	if err != nil {
		return err
	}
	result, err := w.Platform.ExecContext(ctx, `UPDATE migration_jobs SET batches_done=$2,rows_done=$3,heartbeat_at=NOW()
 WHERE id=$1 AND attempt=$4 AND status='running'`, j.ID, batches, rows, j.Attempt)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLostLease
	}
	return nil
}
func (w *Worker) deferBusy(ctx context.Context, j Job) error {
	status := Retrying
	if j.Attempt >= j.MaxAttempts {
		status = Failed
		if j.BatchesDone > 0 {
			status = Partial
		}
		if j.Operation == Rollback && j.BatchesDone > 0 {
			status = PartiallyReverted
		}
	}
	_, err := w.Platform.ExecContext(ctx, `UPDATE migration_jobs SET status=$3,last_error=NULL,available_at=NOW()+INTERVAL '2 seconds',finished_at=CASE WHEN $3 IN ('failed','partial','partially_reverted') THEN NOW() ELSE NULL END WHERE id=$1 AND attempt=$2`, j.ID, j.Attempt, status)
	if err != nil {
		return err
	}
	outcome := string(status)
	if status == Retrying {
		outcome = "lock_busy"
	}
	_, err = w.Platform.ExecContext(ctx, `UPDATE migration_job_attempts SET ended_at=NOW(),outcome=$3 WHERE job_id=$1 AND attempt=$2`, j.ID, j.Attempt, outcome)
	return err
}
func (w *Worker) uncertain(ctx context.Context, j Job, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ctx = cleanup
	_, e := w.Platform.ExecContext(ctx, `UPDATE migration_jobs SET status='uncertain',last_error=$2 WHERE id=$1 AND attempt=$3`, j.ID, truncateError(err), j.Attempt)
	if e != nil {
		return e
	}
	_, e = w.Platform.ExecContext(ctx, `UPDATE migration_job_attempts SET ended_at=NOW(),outcome='uncertain',error_text=$3,sqlstate=$4 WHERE job_id=$1 AND attempt=$2`, j.ID, j.Attempt, truncateError(err), sqlState(err))
	return e
}
func (w *Worker) finish(ctx context.Context, j Job, cause error, transient bool) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ctx = cleanup
	status := Failed
	var committed int64
	_ = w.Platform.QueryRowContext(ctx, `SELECT batches_done FROM migration_jobs WHERE id=$1`, j.ID).Scan(&committed)
	if committed > 0 {
		status = Partial
		if j.Operation == Rollback {
			status = PartiallyReverted
		}
	}
	if transient && j.Attempt < j.MaxAttempts {
		status = Retrying
	}
	log := w.logger().With("job_id", j.ID, "execution_id", j.ExecutionID, "database", j.DatabaseID, "actor", j.ActorID)
	log.Error("job_attempt_failed", "status", status, "error", cause, "attempt", j.Attempt)
	// Failed attempts remain in migration_job_attempts with their original SQL,
	// revision, actor, and error, independent of target transaction rollback.
	result, err := w.Platform.ExecContext(ctx, `UPDATE migration_jobs SET status=$2,last_error=$3,available_at=NOW()+($4 * INTERVAL '1 second'),finished_at=CASE WHEN $2 IN ('failed') THEN NOW() ELSE NULL END
 WHERE id=$1 AND attempt=$5`, j.ID, status, truncateError(cause), int(Backoff(j.Attempt).Seconds()), j.Attempt)
	if err != nil {
		return err
	}
	if affected, e := result.RowsAffected(); e != nil {
		return e
	} else if affected != 1 {
		return ErrLostLease
	}
	if status == Failed && j.OperationReviewID > 0 {
		if _, err = w.Platform.ExecContext(ctx, `UPDATE migration_operation_reviews SET workflow_status='failed' WHERE id=$1 AND workflow_status IN ('approved','queued')`, j.OperationReviewID); err != nil {
			return err
		}
	}
	_, err = w.Platform.ExecContext(ctx, `UPDATE migration_job_attempts SET ended_at=NOW(),outcome=$3,error_text=$4,sqlstate=$5 WHERE job_id=$1 AND attempt=$2`, j.ID, j.Attempt, status, truncateError(cause), sqlState(cause))
	return err
}
func truncateError(e error) string {
	s := e.Error()
	if len(s) > 1500 {
		return s[:1500]
	}
	return s
}
func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}
func (w *Worker) succeed(ctx context.Context, j Job) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ctx = cleanup
	tx, err := w.Platform.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var running bool
	err = tx.QueryRowContext(ctx, `SELECT TRUE FROM migration_jobs WHERE id=$1 AND attempt=$2 AND status='running' FOR UPDATE`, j.ID, j.Attempt).Scan(&running)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLostLease
	}
	if err != nil {
		return err
	}
	// Idempotent history; if target committed but the worker crashed before
	// publishing the result, proof reconciliation re-enters here safely.
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_worker_history
 (execution_id,job_id,database_id,migration_id,revision,actor_id,operation,source_execution_id,sql_text)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(execution_id) DO NOTHING`,
		j.ExecutionID, j.ID, j.DatabaseID, j.MigrationID, j.Revision, j.ActorID, j.Operation, nullable(j.SourceExecutionID),
		effectiveSQL(j))
	if err != nil {
		return err
	}
	if j.Operation == Apply {
		// Commit workflow metadata atomically with job history in the platform DB.
		// The target proof ledger remains the source of truth for target-side commit.
		result, e := tx.ExecContext(ctx, `UPDATE migration_revisions SET workflow_status='executed',
    executed_at=NOW(),execution_id=$3 WHERE definition_id=$1 AND revision=$2
    AND workflow_status='approved' AND executed_at IS NULL`, j.MigrationID, j.Revision, j.ExecutionID)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return fmt.Errorf("approved revision not eligible for executed state: %d", n)
		}
		if _, e = tx.ExecContext(ctx, `UPDATE migration_definitions SET workflow_status='executed',updated_at=NOW() WHERE id=$1`, j.MigrationID); e != nil {
			return e
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE migration_jobs SET status='success',finished_at=NOW(),last_error=NULL WHERE id=$1 AND attempt=$2 AND status='running'`, j.ID, j.Attempt)
	if err != nil {
		return err
	}
	if affected, e := result.RowsAffected(); e != nil {
		return e
	} else if affected != 1 {
		return ErrLostLease
	}
	if j.OperationReviewID > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_operation_reviews SET workflow_status='executed',job_id=COALESCE(job_id,$2)
			WHERE id=$1 AND workflow_status IN ('approved','queued') AND (job_id IS NULL OR job_id=$2)`, j.OperationReviewID, j.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE migration_job_attempts SET ended_at=NOW(),outcome='success' WHERE job_id=$1 AND attempt=$2`, j.ID, j.Attempt)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func effectiveSQL(j Job) string {
	if j.Operation == Rollback {
		return j.ReverseSQL
	}
	return j.SQL
}
func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidJob) || errors.Is(err, ErrUnapproved) || errors.Is(err, ErrUncertain) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return strings.HasPrefix(pg.Code, "08") || pg.Code == "40001" || pg.Code == "40P01" || pg.Code == "55P03" || pg.Code == "57014"
	}
	return false
}

// startHeartbeat keeps the queue lease alive independently from the target
// connection (which can be occupied by a long-running SQL statement).
func (w *Worker) startHeartbeat(ctx context.Context, j Job, log *slog.Logger, cancelExecution context.CancelFunc) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		interval := w.HeartbeatInterval
		if interval <= 0 {
			interval = 20 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				pulseCtx, stop := context.WithTimeout(hbCtx, 5*time.Second)
				err := w.heartbeat(pulseCtx, j)
				stop()
				if err != nil {
					log.Error("job_heartbeat_failed", "error", err)
					cancelExecution()
				}
			}
		}
	}()
	return func() { cancel(); done.Wait() }
}
