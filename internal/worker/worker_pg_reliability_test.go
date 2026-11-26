package worker

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/platformdb"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// These tests require real PostgreSQL. They do not mock commits, job claims, or checkpoint storage.
// Run with WORKER_TEST_PLATFORM_DATABASE_URL and WORKER_TEST_TARGET_DATABASE_URL pointing to
// disposable databases whose names end with _test. Never point these at production databases.
type workerPGFixture struct {
	platform, target   *sql.DB
	worker             *Worker
	userID, databaseID int64
	table              string
}

func requireWorkerTestDSN(t *testing.T, key string) string {
	t.Helper()
	dsn := os.Getenv(key)
	if dsn == "" {
		t.Skipf("set %s to enable PostgreSQL reliability tests", key)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("%s must reference a disposable database ending in _test", key)
	}
	return dsn
}

func workerPGSetup(t *testing.T) *workerPGFixture {
	t.Helper()
	platformDSN := requireWorkerTestDSN(t, "WORKER_TEST_PLATFORM_DATABASE_URL")
	targetDSN := requireWorkerTestDSN(t, "WORKER_TEST_TARGET_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := sql.Open("pgx", platformDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	target, err := sql.Open("pgx", targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if err := platform.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := target.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	tag, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	tag = tag[:12]
	table := "worker_case_" + tag
	f := &workerPGFixture{platform: platform, target: target, table: table}
	if err := platform.QueryRowContext(ctx, `INSERT INTO users(username,password_hash) VALUES($1,'test') RETURNING id`, "worker_test_"+tag).Scan(&f.userID); err != nil {
		t.Fatal(err)
	}
	if err := platform.QueryRowContext(ctx, `INSERT INTO database_connections(name) VALUES($1) RETURNING id`, "worker_db_"+tag).Scan(&f.databaseID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = platform.ExecContext(c, `DELETE FROM migration_job_attempts WHERE job_id IN (SELECT id FROM migration_jobs WHERE database_id=$1)`, f.databaseID)
		_, _ = platform.ExecContext(c, `DELETE FROM migration_worker_history WHERE database_id=$1`, f.databaseID)
		_, _ = platform.ExecContext(c, `DELETE FROM migration_jobs WHERE database_id=$1`, f.databaseID)
		_, _ = platform.ExecContext(c, `DELETE FROM database_connections WHERE id=$1`, f.databaseID)
		_, _ = platform.ExecContext(c, `DELETE FROM users WHERE id=$1`, f.userID)
		_, _ = target.ExecContext(c, `DROP TABLE IF EXISTS "`+f.table+`" CASCADE`)
	})
	f.worker = &Worker{Platform: platform, Targets: TargetResolverFunc(func(id int64) (*sql.DB, error) {
		if id != f.databaseID {
			return nil, fmt.Errorf("unknown database %d", id)
		}
		return target, nil
	}), Validator: ValidatorFunc(func(_ context.Context, j Job) error {
		if j.DatabaseID != f.databaseID || j.ActorID != f.userID {
			return ErrUnapproved
		}
		return nil
	})}
	return f
}

func (f *workerPGFixture) enqueue(t *testing.T, op Operation, mode Mode, sqlText, down, source string, batchSize int) Job {
	t.Helper()
	j, err := Enqueue(context.Background(), f.platform, Job{
		DatabaseID: f.databaseID, MigrationID: 1, Revision: 1, ActorID: f.userID,
		Operation: op, Mode: mode, SQL: sqlText, ReverseSQL: down,
		BatchTable: func() string {
			if mode == Batch {
				return f.table
			}
			return ""
		}(),
		BatchSize: batchSize, SourceExecutionID: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func (f *workerPGFixture) runOnce(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := f.worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}
func (f *workerPGFixture) state(t *testing.T, j Job) (Status, int, int64, int64) {
	t.Helper()
	var status Status
	var attempts int
	var batches, rows int64
	if err := f.platform.QueryRow(`SELECT status,attempt,batches_done,rows_done FROM migration_jobs WHERE id=$1`, j.ID).Scan(&status, &attempts, &batches, &rows); err != nil {
		t.Fatal(err)
	}
	return status, attempts, batches, rows
}

func TestWorkerPGNormalIdempotency(t *testing.T) {
	f := workerPGSetup(t)
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `" (id bigint PRIMARY KEY, counter integer NOT NULL DEFAULT 0); INSERT INTO "` + f.table + `"(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	j := f.enqueue(t, Forward, Normal, `UPDATE "`+f.table+`" SET counter=counter+1 WHERE id=1`, "", "", 0)
	f.runOnce(t)
	f.runOnce(t) // The queue must not replay an already successful job.
	var count int
	if err := f.target.QueryRow(`SELECT counter FROM "` + f.table + `" WHERE id=1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("executed non-idempotent SQL %d times, expected once", count)
	}
	status, attempts, _, _ := f.state(t, j)
	if status != Success || attempts != 1 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
	var proof, history int
	if err := f.target.QueryRow(`SELECT COUNT(*) FROM __dbmp_worker_proof WHERE execution_id=$1 AND batch_no=0`, j.ExecutionID).Scan(&proof); err != nil {
		t.Fatal(err)
	}
	if err := f.platform.QueryRow(`SELECT COUNT(*) FROM migration_worker_history WHERE execution_id=$1`, j.ExecutionID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if proof != 1 || history != 1 {
		t.Fatalf("proof=%d history=%d; expected exactly one each", proof, history)
	}
}

func TestWorkerPGBatchAndReverseOrderRollback(t *testing.T) {
	f := workerPGSetup(t)
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `"(id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false); INSERT INTO "` + f.table + `"(id) SELECT generate_series(1,12)`); err != nil {
		t.Fatal(err)
	}
	up := `UPDATE "` + f.table + `" SET processed=true WHERE id=ANY($1::bigint[]) AND processed=false`
	down := `UPDATE "` + f.table + `" SET processed=false WHERE id=ANY($1::bigint[]) AND processed=true`
	apply := f.enqueue(t, Forward, Batch, up, down, "", 5)
	f.runOnce(t)
	status, _, batches, rows := f.state(t, apply)
	if status != Success || batches != 3 || rows != 12 {
		t.Fatalf("apply status=%s batches=%d rows=%d", status, batches, rows)
	}
	var processed int
	if err := f.target.QueryRow(`SELECT count(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed != 12 {
		t.Fatalf("processed=%d, expected 12", processed)
	}
	rollback := f.enqueue(t, Rollback, Batch, up, down, apply.ExecutionID, 5)
	f.runOnce(t)
	status, _, batches, rows = f.state(t, rollback)
	if status != Success || batches != 3 || rows != 12 {
		t.Fatalf("rollback status=%s batches=%d rows=%d", status, batches, rows)
	}
	if err := f.target.QueryRow(`SELECT count(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Fatalf("rollback left %d rows processed", processed)
	}
}

func TestWorkerPGPartialCompensationResumesFromReverseCheckpoint(t *testing.T) {
	f := workerPGSetup(t)
	gate := f.table + "_gate"
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `"(id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false);
		INSERT INTO "` + f.table + `"(id) SELECT generate_series(1,12);
		CREATE TABLE "` + gate + `"(fail boolean NOT NULL);
		INSERT INTO "` + gate + `" VALUES (true)`); err != nil {
		t.Fatal(err)
	}
	up := `UPDATE "` + f.table + `" SET processed=true WHERE id=ANY($1::bigint[])`
	down := `UPDATE "` + f.table + `" SET processed=CASE WHEN id<=5 AND EXISTS(SELECT 1 FROM "` + gate + `" WHERE fail) THEN (id/(id-id))=1 ELSE false END WHERE id=ANY($1::bigint[])`
	apply := f.enqueue(t, Forward, Batch, up, down, "", 5)
	f.runOnce(t)
	if status, _, batches, rows := f.state(t, apply); status != Success || batches != 3 || rows != 12 {
		t.Fatalf("source apply status=%s batches=%d rows=%d", status, batches, rows)
	}
	rollback := f.enqueue(t, Rollback, Batch, up, down, apply.ExecutionID, 5)
	f.runOnce(t)
	status, attempts, batches, rows := f.state(t, rollback)
	if status != PartiallyReverted || attempts != 1 || batches != 2 || rows != 7 {
		t.Fatalf("partial compensation status=%s attempts=%d batches=%d rows=%d", status, attempts, batches, rows)
	}
	var processed int
	if err := f.target.QueryRow(`SELECT count(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil || processed != 5 {
		t.Fatalf("first compensation changed unexpected rows: processed=%d err=%v", processed, err)
	}
	if _, err := f.target.Exec(`DELETE FROM "` + gate + `"`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.platform.Exec(`UPDATE migration_jobs SET status='retrying',max_attempts=8,available_at=NOW(),finished_at=NULL WHERE id=$1 AND status='partially_reverted'`, rollback.ID); err != nil {
		t.Fatal(err)
	}
	f.runOnce(t)
	status, attempts, batches, rows = f.state(t, rollback)
	if status != Success || attempts != 2 || batches != 3 || rows != 12 {
		t.Fatalf("resumed compensation status=%s attempts=%d batches=%d rows=%d", status, attempts, batches, rows)
	}
	if err := f.target.QueryRow(`SELECT count(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil || processed != 0 {
		t.Fatalf("compensation resume left changed rows: processed=%d err=%v", processed, err)
	}
}

func TestWorkerPGStaleClaimProofRecovery(t *testing.T) {
	f := workerPGSetup(t)
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `"(id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false); INSERT INTO "` + f.table + `"(id) SELECT generate_series(1,7)`); err != nil {
		t.Fatal(err)
	}
	up := `UPDATE "` + f.table + `" SET processed=true WHERE id=ANY($1::bigint[]) AND processed=false`
	j := f.enqueue(t, Forward, Batch, up, "", "", 3)
	// Simulate a crash AFTER the first target transaction committed and before
	// the platform worker recorded its successful completion.
	if _, err := f.target.Exec(`CREATE TABLE IF NOT EXISTS __dbmp_worker_proof (
       execution_id TEXT NOT NULL,batch_no BIGINT NOT NULL,source_execution_id TEXT,
       batch_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
       committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(execution_id,batch_no))`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.target.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE "`+f.table+`" SET processed=true WHERE id=ANY($1::bigint[])`, []int64{1, 2, 3}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO __dbmp_worker_proof(execution_id,batch_no,batch_ids) VALUES($1,-1,'[7]'::jsonb),($1,0,'[1,2,3]'::jsonb)`, j.ExecutionID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.platform.Exec(`UPDATE migration_jobs SET status='running',attempt=1,heartbeat_at=NOW()-INTERVAL '6 minutes' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	// New worker must use persisted proof and only process remaining IDs.
	f.runOnce(t)
	status, attempts, batches, rows := f.state(t, j)
	if status != Success || attempts != 2 || batches != 3 || rows != 7 {
		t.Fatalf("recovery status=%s attempts=%d batches=%d rows=%d", status, attempts, batches, rows)
	}
	var processed int
	if err := f.target.QueryRow(`SELECT count(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed != 7 {
		t.Fatalf("after recovery processed=%d, expected 7", processed)
	}
}

func TestWorkerPGPartialCommitAndFailedAttemptAudit(t *testing.T) {
	f := workerPGSetup(t)
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `"(id bigint PRIMARY KEY, processed boolean NOT NULL DEFAULT false); INSERT INTO "` + f.table + `"(id) SELECT generate_series(1,9)`); err != nil {
		t.Fatal(err)
	}
	// Batch #0 (IDs 1..5) commits. Batch #1 raises division by zero and must roll back.
	// The expression depends on a column to avoid constant-folding at planning time.
	up := `UPDATE "` + f.table + `" SET processed=CASE WHEN id>5 THEN (id / (id-id))=1 ELSE true END WHERE id=ANY($1::bigint[])`
	j := f.enqueue(t, Forward, Batch, up, "", "", 5)
	f.runOnce(t)
	status, attempts, batches, rows := f.state(t, j)
	if status != Partial || attempts != 1 || batches != 1 || rows != 5 {
		t.Fatalf("partial status=%s attempts=%d batches=%d rows=%d", status, attempts, batches, rows)
	}
	f.runOnce(t) // Permanent SQL failure must not auto-retry a partial execution.
	statusAfter, attemptsAfter, _, _ := f.state(t, j)
	if statusAfter != Partial || attemptsAfter != attempts {
		t.Fatalf("partial job retried automatically: status=%s attempts=%d", statusAfter, attemptsAfter)
	}
	var processed int
	if err := f.target.QueryRow(`SELECT COUNT(*) FROM "` + f.table + `" WHERE processed`).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed != 5 {
		t.Fatalf("partial commit changed %d rows, expected 5", processed)
	}
	var outcome string
	var errText sql.NullString
	if err := f.platform.QueryRow(`SELECT outcome,error_text FROM migration_job_attempts WHERE job_id=$1 AND attempt=1`, j.ID).Scan(&outcome, &errText); err != nil {
		t.Fatal(err)
	}
	if outcome != string(Partial) || !errText.Valid || errText.String == "" {
		t.Fatalf("failed attempt not recorded: outcome=%s error=%v", outcome, errText)
	}
	// The next attempt will encounter the same error; this test intentionally
	// stops here rather than silently modifying immutable SQL to fake recovery.
}

func TestWorkerPGTwoConsumersSingleExecution(t *testing.T) {
	f := workerPGSetup(t)
	if _, err := f.target.Exec(`CREATE TABLE "` + f.table + `"(id bigint PRIMARY KEY,counter integer NOT NULL DEFAULT 0); INSERT INTO "` + f.table + `"(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	j := f.enqueue(t, Forward, Normal, `UPDATE "`+f.table+`" SET counter=counter+1 WHERE id=1`, "", "", 0)
	results := make(chan error, 2)
	for k := 0; k < 2; k++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			results <- f.worker.RunOnce(ctx)
		}()
	}
	for k := 0; k < 2; k++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent worker: %v", err)
		}
	}
	var count int
	if err := f.target.QueryRow(`SELECT counter FROM "` + f.table + `" WHERE id=1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	status, attempts, _, _ := f.state(t, j)
	if count != 1 || status != Success || attempts != 1 {
		t.Fatalf("count=%d status=%s attempts=%d", count, status, attempts)
	}
}
