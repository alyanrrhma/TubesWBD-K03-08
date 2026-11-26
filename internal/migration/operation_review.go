package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type OperationReviewGroup struct {
	GroupID      int64  `json:"group_id"`
	GroupName    string `json:"group_name"`
	LeaderUserID int64  `json:"leader_user_id"`
	Decision     string `json:"decision"`
}

type OperationReview struct {
	ID                int64                  `json:"id"`
	DatabaseID        int64                  `json:"database_id"`
	MigrationID       int64                  `json:"migration_id"`
	Revision          int64                  `json:"revision"`
	Operation         string                 `json:"operation"`
	SourceExecutionID string                 `json:"source_execution_id"`
	ActorID           int64                  `json:"actor_id"`
	Status            string                 `json:"status"`
	Groups            []OperationReviewGroup `json:"groups"`
}

func (s *WorkflowService) StartOperationReview(ctx context.Context, userID, definitionID int64, operation worker.Operation) (OperationReview, error) {
	if operation != worker.Rollback && operation != worker.Forward {
		return OperationReview{}, ErrInvalidArgument
	}
	definition, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return OperationReview{}, err
	}
	if err = s.canEdit(ctx, userID, definition.DatabaseID); err != nil {
		return OperationReview{}, err
	}
	ops, err := s.ExecutionOperations(ctx, userID, definitionID)
	if err != nil {
		return OperationReview{}, err
	}
	availability := ops.Rollback
	if operation == worker.Forward {
		availability = ops.Forward
	}
	if !availability.Available {
		return OperationReview{}, fmt.Errorf("%w: %s", ErrWorkflowInvalid, availability.Reason)
	}
	revision, err := s.Repo.GetRevision(ctx, definitionID, ops.Revision)
	if err != nil {
		return OperationReview{}, err
	}
	tx, err := s.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return OperationReview{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationSubmitLockID); err != nil {
		return OperationReview{}, err
	}
	var existingID int64
	existingErr := tx.QueryRowContext(ctx, `SELECT id FROM migration_operation_reviews WHERE migration_id=$1 AND operation=$2 AND source_execution_id=$3
		AND workflow_status IN ('in_review','approved','queued') ORDER BY created_at DESC LIMIT 1`, definitionID, operation, ops.LastExecutionID).Scan(&existingID)
	if existingErr == nil {
		if err = tx.Commit(); err != nil {
			return OperationReview{}, err
		}
		return s.GetOperationReview(ctx, userID, existingID)
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return OperationReview{}, existingErr
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_operation_reviews(database_id,migration_id,revision,operation,source_execution_id,actor_id,up_sql,down_sql)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, definition.DatabaseID, definitionID, revision.Revision, operation, ops.LastExecutionID, userID, revision.UpSQL, revision.DownSQL).Scan(&id)
	if err != nil {
		return OperationReview{}, fmt.Errorf("create operation review: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT ga.group_id,g.leader_user_id,ga.access_level FROM group_database_access ga JOIN app_groups g ON g.id=ga.group_id WHERE ga.database_id=$1 ORDER BY ga.group_id FOR SHARE OF ga,g`, definition.DatabaseID)
	if err != nil {
		return OperationReview{}, err
	}
	type approver struct {
		group       int64
		leader      sql.NullInt64
		accessLevel string
	}
	var groups []approver
	for rows.Next() {
		var a approver
		if err = rows.Scan(&a.group, &a.leader, &a.accessLevel); err != nil {
			rows.Close()
			return OperationReview{}, err
		}
		groups = append(groups, a)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return OperationReview{}, err
	}
	if err = rows.Close(); err != nil {
		return OperationReview{}, err
	}
	if len(groups) == 0 {
		return OperationReview{}, ErrNoApprovalGroups
	}
	for _, g := range groups {
		if !g.leader.Valid {
			return OperationReview{}, ErrGroupLeaderMissing
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO migration_operation_review_groups(review_id,group_id,leader_user_id,access_level) VALUES($1,$2,$3,$4)`, id, g.group, g.leader.Int64, g.accessLevel); err != nil {
			return OperationReview{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return OperationReview{}, err
	}
	return s.GetOperationReview(ctx, userID, id)
}

func (s *WorkflowService) GetOperationReview(ctx context.Context, userID, reviewID int64) (OperationReview, error) {
	var r OperationReview
	err := s.Repo.DB.QueryRowContext(ctx, `SELECT id,database_id,migration_id,revision,operation,source_execution_id,actor_id,workflow_status FROM migration_operation_reviews WHERE id=$1`, reviewID).
		Scan(&r.ID, &r.DatabaseID, &r.MigrationID, &r.Revision, &r.Operation, &r.SourceExecutionID, &r.ActorID, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrWorkflowNotFound
	}
	if err != nil {
		return r, err
	}
	if err = s.canView(ctx, userID, r.DatabaseID); err != nil {
		return OperationReview{}, err
	}
	rows, err := s.Repo.DB.QueryContext(ctx, `SELECT rg.group_id,g.name,rg.leader_user_id,COALESCE(a.decision,'pending') FROM migration_operation_review_groups rg JOIN app_groups g ON g.id=rg.group_id LEFT JOIN migration_operation_approvals a ON a.review_id=rg.review_id AND a.group_id=rg.group_id WHERE rg.review_id=$1 ORDER BY lower(g.name),rg.group_id`, reviewID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	r.Groups = make([]OperationReviewGroup, 0)
	for rows.Next() {
		var g OperationReviewGroup
		if err = rows.Scan(&g.GroupID, &g.GroupName, &g.LeaderUserID, &g.Decision); err != nil {
			return r, err
		}
		r.Groups = append(r.Groups, g)
	}
	return r, rows.Err()
}

func (s *WorkflowService) GetLatestOperationReview(ctx context.Context, userID, definitionID int64, operation worker.Operation) (OperationReview, error) {
	if operation != worker.Rollback && operation != worker.Forward {
		return OperationReview{}, ErrInvalidArgument
	}
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return OperationReview{}, err
	}
	if err = s.canView(ctx, userID, d.DatabaseID); err != nil {
		return OperationReview{}, err
	}
	var id int64
	err = s.Repo.DB.QueryRowContext(ctx, `SELECT id FROM migration_operation_reviews WHERE migration_id=$1 AND operation=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, definitionID, operation).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationReview{}, ErrWorkflowNotFound
	}
	if err != nil {
		return OperationReview{}, err
	}
	return s.GetOperationReview(ctx, userID, id)
}

func (s *WorkflowService) DecideOperationReview(ctx context.Context, userID, reviewID, groupID int64, decision string) (OperationReview, error) {
	if userID <= 0 || reviewID <= 0 || groupID <= 0 || (decision != "approved" && decision != "rejected") {
		return OperationReview{}, ErrInvalidArgument
	}
	tx, err := s.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return OperationReview{}, err
	}
	defer tx.Rollback()
	var databaseID int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT database_id,workflow_status FROM migration_operation_reviews WHERE id=$1 FOR UPDATE`, reviewID).Scan(&databaseID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationReview{}, ErrWorkflowNotFound
	}
	if err != nil {
		return OperationReview{}, err
	}
	if err = s.canView(ctx, userID, databaseID); err != nil {
		return OperationReview{}, err
	}
	if status != "in_review" {
		return OperationReview{}, ErrWorkflowInvalid
	}
	var leader int64
	err = tx.QueryRowContext(ctx, `SELECT rg.leader_user_id FROM migration_operation_review_groups rg JOIN group_database_access ga ON ga.group_id=rg.group_id AND ga.database_id=$2 JOIN app_groups g ON g.id=rg.group_id AND g.leader_user_id=rg.leader_user_id JOIN user_groups ug ON ug.group_id=g.id AND ug.user_id=rg.leader_user_id WHERE rg.review_id=$1 AND rg.group_id=$3 FOR UPDATE OF rg`, reviewID, databaseID, groupID).Scan(&leader)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationReview{}, ErrNotGroupLeader
	}
	if err != nil {
		return OperationReview{}, err
	}
	if leader != userID {
		return OperationReview{}, ErrNotGroupLeader
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_operation_approvals(review_id,group_id,user_id,decision) VALUES($1,$2,$3,$4)`, reviewID, groupID, userID, decision)
	if err != nil {
		return OperationReview{}, ErrApprovalDuplicate
	}
	if decision == "rejected" {
		_, err = tx.ExecContext(ctx, `UPDATE migration_operation_reviews SET workflow_status='rejected' WHERE id=$1`, reviewID)
	} else {
		var complete bool
		err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM migration_operation_review_groups rg LEFT JOIN migration_operation_approvals a ON a.review_id=rg.review_id AND a.group_id=rg.group_id AND a.decision='approved' WHERE rg.review_id=$1 AND a.id IS NULL)`, reviewID).Scan(&complete)
		if err == nil && complete {
			_, err = tx.ExecContext(ctx, `UPDATE migration_operation_reviews SET workflow_status='approved' WHERE id=$1`, reviewID)
		}
	}
	if err != nil {
		return OperationReview{}, err
	}
	if err = tx.Commit(); err != nil {
		return OperationReview{}, err
	}
	return s.GetOperationReview(ctx, userID, reviewID)
}

func (s *WorkflowService) SubmitOperationReview(ctx context.Context, userID, reviewID int64) (worker.Job, error) {
	var job worker.Job
	var status string
	err := s.Repo.DB.QueryRowContext(ctx, `SELECT id,database_id,migration_id,revision,operation,source_execution_id,up_sql,down_sql,workflow_status FROM migration_operation_reviews WHERE id=$1`, reviewID).
		Scan(&job.OperationReviewID, &job.DatabaseID, &job.MigrationID, &job.Revision, &job.Operation, &job.SourceExecutionID, &job.SQL, &job.ReverseSQL, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return worker.Job{}, ErrWorkflowNotFound
	}
	if err != nil {
		return worker.Job{}, err
	}
	if err = s.canEdit(ctx, userID, job.DatabaseID); err != nil {
		return worker.Job{}, err
	}
	if status != "approved" {
		return worker.Job{}, ErrApprovalIncomplete
	}
	job.ActorID = userID
	mode, table, size, err := approvedBatchConfig(job.SQL)
	if err != nil {
		return worker.Job{}, err
	}
	job.Mode, job.BatchTable, job.BatchSize = mode, table, size
	if mode == worker.Batch && !strings.Contains(job.ReverseSQL, "$1") {
		return worker.Job{}, fmt.Errorf("batch down_sql must consume $1 ID array")
	}
	job.ExecutionID, err = newExecutionID()
	if err != nil {
		return worker.Job{}, err
	}
	queued, err := s.enqueueApprovedOperation(ctx, job)
	if err != nil {
		return worker.Job{}, err
	}
	return queued, nil
}
