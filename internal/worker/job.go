// Package worker implements the durable PostgreSQL-backed migration execution engine.
// The HTTP/API domain owns authentication, authorization, approvals and immutable revisions.
package worker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/telemetry"
	"regexp"
	"strings"
	"time"
)

type Operation string

const (
	Apply    Operation = "apply"
	Forward  Operation = "forward"
	Rollback Operation = "rollback"
)

type Mode string

const (
	Normal Mode = "normal"
	Batch  Mode = "batch"
)

type Status string

const (
	Pending           Status = "pending"
	Running           Status = "running"
	Retrying          Status = "retrying"
	Success           Status = "success"
	Failed            Status = "failed"
	Partial           Status = "partial"
	PartiallyReverted Status = "partially_reverted"
	Uncertain         Status = "uncertain"
)

var (
	ErrInvalidJob = errors.New("invalid migration job")
	ErrUnapproved = errors.New("job must be revalidated against current approvals and immutable revision")
	ErrUncertain  = errors.New("transaction commit outcome uncertain; manual reconciliation required")
)

// Job contains an immutable approved statement snapshot, NOT an SQL statement from the queue caller.
// For batch, SQL must use $1::bigint[] for the explicit row IDs and affect every provided ID.
// BatchTable must be a single trusted, fixed database table name (no schema-qualified names).
// The same invariant applies to ReverseSQL; irreversible migrations cannot be rolled back.
type Job struct {
	ID, ExecutionID                                             string
	OperationReviewID                                           int64
	DatabaseID, MigrationID, Revision, ActorID                  int64
	Operation                                                   Operation
	Mode                                                        Mode
	SQL, ReverseSQL, BatchTable, SourceExecutionID, Traceparent string
	BatchSize, MaxAttempts                                      int
	Status                                                      Status
	Attempt                                                     int
	BatchesDone, RowsDone                                       int64
	QueueWaitMS                                                 int64
}

var identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]{0,62}$`)

func (j Job) Validate() error {
	if j.DatabaseID <= 0 || j.MigrationID <= 0 || j.Revision <= 0 || j.ActorID <= 0 || strings.TrimSpace(j.SQL) == "" || len(j.SQL) > 1<<20 {
		return ErrInvalidJob
	}
	if j.Operation != Apply && j.Operation != Forward && j.Operation != Rollback {
		return ErrInvalidJob
	}
	if j.Mode != Normal && j.Mode != Batch {
		return ErrInvalidJob
	}
	if j.Operation == Rollback && (j.SourceExecutionID == "" || strings.TrimSpace(j.ReverseSQL) == "") {
		return ErrInvalidJob
	}
	if j.Mode == Batch && (!identifier.MatchString(j.BatchTable) || j.BatchSize < 1 || j.BatchSize > 100000) {
		return ErrInvalidJob
	}
	if j.Mode == Normal && (j.BatchTable != "" || j.BatchSize != 0) {
		return ErrInvalidJob
	}
	if j.MaxAttempts < 0 || j.MaxAttempts > 20 {
		return ErrInvalidJob
	}
	return nil
}
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Enqueue is the API-facing queue publisher. Domain validation MUST occur before
// Enqueue; the worker independently repeats it using Validator.
func Enqueue(ctx context.Context, db *sql.DB, j Job) (Job, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("begin enqueue: %w", err)
	}
	defer tx.Rollback()
	queued, err := EnqueueTx(ctx, tx, j)
	if err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("commit enqueue: %w", err)
	}
	return queued, nil
}

// EnqueueTx writes a job in the caller's transaction so review state and queue
// publication can become visible atomically.
func EnqueueTx(ctx context.Context, tx *sql.Tx, j Job) (Job, error) {
	if err := j.Validate(); err != nil {
		return Job{}, err
	}
	if j.ID == "" {
		id, err := newID()
		if err != nil {
			return Job{}, err
		}
		j.ID = id
	}
	if j.ExecutionID == "" {
		id, err := newID()
		if err != nil {
			return Job{}, err
		}
		j.ExecutionID = id
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 4
	}
	if j.Traceparent == "" {
		j.Traceparent = telemetry.Traceparent(ctx)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO migration_jobs
 (id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,reverse_sql,batch_table,batch_size,source_execution_id,traceparent,max_attempts,operation_review_id)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		j.ID, j.ExecutionID, j.DatabaseID, j.MigrationID, j.Revision, j.ActorID, j.Operation, j.Mode, j.SQL,
		nullable(j.ReverseSQL), nullable(j.BatchTable), nullableInt(j.BatchSize), nullable(j.SourceExecutionID), j.Traceparent, j.MaxAttempts, nullableInt64(j.OperationReviewID))
	if err != nil {
		return Job{}, fmt.Errorf("enqueue: %w", err)
	}
	j.Status = Pending
	return j, nil
}
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// Validator MUST check permissions, approvals of every database-access group leader,
// revision immutability, active-last/linear ordering, and whether DB is blocked.
// Fail closed: no execution is permitted without a validator.
type Validator interface {
	Revalidate(context.Context, Job) error
}
type ValidatorFunc func(context.Context, Job) error

func (f ValidatorFunc) Revalidate(ctx context.Context, j Job) error { return f(ctx, j) }

type TargetResolver interface{ Get(int64) (*sql.DB, error) }
type TargetResolverFunc func(int64) (*sql.DB, error)

func (f TargetResolverFunc) Get(id int64) (*sql.DB, error) { return f(id) }

// Backoff bounds exponential retry delays. Testable without sleeping.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	return time.Second * time.Duration(1<<uint(attempt-1))
}
