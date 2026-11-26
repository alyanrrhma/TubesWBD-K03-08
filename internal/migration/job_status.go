package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type JobStatus struct {
	ID          string     `json:"job_id"`
	ExecutionID string     `json:"execution_id"`
	DatabaseID  int64      `json:"database_id"`
	MigrationID int64      `json:"migration_id"`
	Operation   string     `json:"operation"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	Attempt     int        `json:"attempt"`
	BatchesDone int64      `json:"batches_done"`
	RowsDone    int64      `json:"rows_done"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CanResume   bool       `json:"can_resume"`
}

func (s *WorkflowService) GetJobStatus(ctx context.Context, actorID int64, jobID string) (JobStatus, error) {
	var j JobStatus
	var errorText sql.NullString
	err := s.Repo.DB.QueryRowContext(ctx, `SELECT id,execution_id,database_id,migration_id,operation,mode,status,attempt,batches_done,rows_done,last_error,created_at,finished_at FROM migration_jobs WHERE id=$1`, jobID).Scan(&j.ID, &j.ExecutionID, &j.DatabaseID, &j.MigrationID, &j.Operation, &j.Mode, &j.Status, &j.Attempt, &j.BatchesDone, &j.RowsDone, &errorText, &j.CreatedAt, &j.FinishedAt)
	if err == sql.ErrNoRows {
		return j, ErrWorkflowNotFound
	}
	if err != nil {
		return j, fmt.Errorf("read job status: %w", err)
	}
	if err = s.canEdit(ctx, actorID, j.DatabaseID); err != nil {
		return JobStatus{}, err
	}
	j.LastError = errorText.String
	j.CanResume = (j.Status == string(worker.Partial) || j.Status == string(worker.PartiallyReverted)) && j.Attempt < 20
	return j, nil
}

// ResumeJob is an explicit operator action for a permanently partial job. It
// leaves target-side checkpoints intact and only makes the same execution
// eligible to continue; partial work is never retried automatically.
func (s *WorkflowService) ResumeJob(ctx context.Context, actorID int64, jobID string) (JobStatus, error) {
	if actorID <= 0 || strings.TrimSpace(jobID) == "" {
		return JobStatus{}, ErrInvalidArgument
	}
	var j worker.Job
	var status string
	err := s.Repo.DB.QueryRowContext(ctx, `SELECT id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,
		COALESCE(reverse_sql,''),COALESCE(batch_table,''),COALESCE(batch_size,0),COALESCE(source_execution_id,''),traceparent,
		max_attempts,attempt,status,batches_done,rows_done FROM migration_jobs WHERE id=$1`, jobID).
		Scan(&j.ID, &j.ExecutionID, &j.DatabaseID, &j.MigrationID, &j.Revision, &j.ActorID, &j.Operation, &j.Mode, &j.SQL, &j.ReverseSQL,
			&j.BatchTable, &j.BatchSize, &j.SourceExecutionID, &j.Traceparent, &j.MaxAttempts, &j.Attempt, &status, &j.BatchesDone, &j.RowsDone)
	if errors.Is(err, sql.ErrNoRows) {
		return JobStatus{}, ErrWorkflowNotFound
	}
	if err != nil {
		return JobStatus{}, fmt.Errorf("load job for resume: %w", err)
	}
	if err = s.canEdit(ctx, actorID, j.DatabaseID); err != nil {
		return JobStatus{}, err
	}
	if status != string(worker.Partial) && status != string(worker.PartiallyReverted) {
		return JobStatus{}, ErrWorkflowInvalid
	}
	if j.Attempt >= 20 {
		return JobStatus{}, fmt.Errorf("%w: retry limit reached; create a corrective migration", ErrWorkflowInvalid)
	}
	if s.Revalidator == nil {
		return JobStatus{}, worker.ErrUnapproved
	}
	if err = s.Revalidator.Revalidate(ctx, j); err != nil {
		return JobStatus{}, err
	}
	result, err := s.Repo.DB.ExecContext(ctx, `UPDATE migration_jobs j SET status='retrying',last_error=NULL,available_at=NOW(),
		max_attempts=LEAST(max_attempts+4,20),finished_at=NULL
		WHERE j.id=$1 AND j.status IN ('partial','partially_reverted')
		AND NOT EXISTS(SELECT 1 FROM migration_jobs other WHERE other.database_id=j.database_id AND other.id<>j.id
			AND other.status IN ('running','partial','partially_reverted','uncertain'))`, jobID)
	if err != nil {
		return JobStatus{}, fmt.Errorf("resume partial job: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return JobStatus{}, err
	}
	if n != 1 {
		return JobStatus{}, ErrDatabaseExecutionBlocked
	}
	slog.Default().Info("migration_job_resumed", "job_id", jobID, "execution_id", j.ExecutionID, "actor", actorID, "database", j.DatabaseID)
	return s.GetJobStatus(ctx, actorID, jobID)
}
