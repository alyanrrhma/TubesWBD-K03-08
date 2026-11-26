package migration

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ExecutionAttempt is the user-visible, append-only execution record. Legacy
// Milestone 1 executions are included so upgrading a platform does not hide its
// existing migration history.
type ExecutionAttempt struct {
	ID                string     `json:"id"`
	MigrationID       int64      `json:"migration_id"`
	ExecutionID       string     `json:"execution_id"`
	JobID             string     `json:"job_id,omitempty"`
	Attempt           int        `json:"attempt"`
	Revision          int64      `json:"revision"`
	ActorID           int64      `json:"actor_id"`
	Actor             string     `json:"actor"`
	DatabaseID        int64      `json:"database_id"`
	Operation         string     `json:"operation"`
	SourceExecutionID string     `json:"source_execution_id,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	Status            string     `json:"status"`
	SQL               string     `json:"sql"`
	Error             string     `json:"error,omitempty"`
	SQLState          string     `json:"sqlstate,omitempty"`
	SQLStarted        bool       `json:"sql_started"`
	Legacy            bool       `json:"legacy,omitempty"`
}

// ListExecutionHistory returns all attempts, not just successful target
// commits. Pagination is chronological (newest first).
func (s *WorkflowService) ListExecutionHistory(ctx context.Context, userID, databaseID int64, page, pageSize int, hideRollback bool) ([]ExecutionAttempt, int, error) {
	if userID <= 0 || databaseID <= 0 || page < 1 || pageSize < 1 || pageSize > maxPageSize {
		return nil, 0, ErrInvalidArgument
	}
	if err := s.canView(ctx, userID, databaseID); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	const cte = `WITH attempts AS (
		SELECT 'job:'||a.job_id||':'||a.attempt AS id,
		       j.migration_id,a.execution_id,a.job_id,a.attempt,a.revision,a.actor_id,u.username,
		       j.database_id,a.operation,COALESCE(j.source_execution_id,'') AS source_execution_id,a.started_at,a.ended_at,
		       COALESCE(a.outcome,j.status) AS status,a.sql_text,COALESCE(a.error_text,j.last_error,'') AS error_text,
	       COALESCE(a.sqlstate,'') AS sqlstate,a.sql_started,FALSE AS legacy
		FROM migration_job_attempts a
		JOIN migration_jobs j ON j.id=a.job_id
		JOIN users u ON u.id=a.actor_id
		WHERE j.database_id=$1
		UNION ALL
		SELECT 'legacy:'||h.id::text,h.id,''::text,''::text,1,h.version,h.executed_by,u.username,
		       h.database_id,'apply'::text,''::text,h.executed_at,h.executed_at,
	       lower(h.status),h.sql_text,COALESCE(h.error_message,''),''::text,TRUE,TRUE
		FROM migration_history h JOIN users u ON u.id=h.executed_by
		WHERE h.database_id=$1
	)`
	var total int
	const filter = ` WHERE NOT $2::boolean OR NOT (
		(operation='rollback' AND status='success') OR
		(operation IN ('apply','forward') AND status='success' AND execution_id IN
		 (SELECT source_execution_id FROM attempts WHERE operation='rollback' AND status='success' AND source_execution_id<>'')))`
	if err := s.Repo.DB.QueryRowContext(ctx, cte+` SELECT COUNT(*) FROM attempts`+filter, databaseID, hideRollback).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count execution history: %w", err)
	}
	rows, err := s.Repo.DB.QueryContext(ctx, cte+` SELECT id,migration_id,execution_id,job_id,attempt,revision,actor_id,username,
		database_id,operation,source_execution_id,started_at,ended_at,status,sql_text,error_text,sqlstate,sql_started,legacy
		FROM attempts`+filter+` ORDER BY started_at DESC,id DESC LIMIT $3 OFFSET $4`, databaseID, hideRollback, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("list execution history: %w", err)
	}
	defer rows.Close()
	items := make([]ExecutionAttempt, 0, pageSize)
	for rows.Next() {
		var item ExecutionAttempt
		var jobID, sourceID, errText, state sql.NullString
		var finished sql.NullTime
		if err := rows.Scan(&item.ID, &item.MigrationID, &item.ExecutionID, &jobID, &item.Attempt, &item.Revision, &item.ActorID, &item.Actor,
			&item.DatabaseID, &item.Operation, &sourceID, &item.StartedAt, &finished, &item.Status, &item.SQL, &errText, &state, &item.SQLStarted, &item.Legacy); err != nil {
			return nil, 0, fmt.Errorf("scan execution history: %w", err)
		}
		item.JobID, item.SourceExecutionID, item.Error, item.SQLState = jobID.String, sourceID.String, errText.String, state.String
		if finished.Valid {
			t := finished.Time
			item.FinishedAt = &t
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate execution history: %w", err)
	}
	return items, total, nil
}
