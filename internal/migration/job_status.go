package migration

import (
	"context"
	"database/sql"
	"fmt"
	"time"
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
	return j, nil
}
