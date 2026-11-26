package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

var (
	ErrMigrationAlreadyExecuted = errors.New("migration definition has already been executed")
	ErrMigrationOrderInvalid    = errors.New("migration is not next in database order")
	ErrDatabaseExecutionBlocked = errors.New("database has a blocked execution")
	ErrExecutionAlreadyQueued   = errors.New("migration revision already has an active or successful job")
	ErrJobSnapshotMismatch      = errors.New("job does not match the approved revision snapshot")
	ErrRollbackUnavailable      = errors.New("migration is not the active last migration")
	ErrForwardUnavailable       = errors.New("migration has no successful rollback to forward")
	ErrIrreversibleRollback     = errors.New("irreversible migration cannot be rolled back")
)

const migrationSubmitLockID int64 = 0x44424d5000000001

// Revalidator enforces domain invariants both before publication and again in
// the worker immediately before target execution.
type Revalidator struct {
	Platform *sql.DB
	Authz    Authorizer
}

func NewRevalidator(platform *sql.DB, authz Authorizer) *Revalidator {
	return &Revalidator{Platform: platform, Authz: authz}
}

var _ worker.Validator = (*Revalidator)(nil)

func (v *Revalidator) Revalidate(ctx context.Context, j worker.Job) error {
	if v == nil || v.Platform == nil || v.Authz == nil {
		return worker.ErrUnapproved
	}
	if (j.Operation != worker.Apply && j.Operation != worker.Rollback && j.Operation != worker.Forward) || (j.Mode != worker.Normal && j.Mode != worker.Batch) || j.DatabaseID <= 0 || j.MigrationID <= 0 || j.Revision <= 0 || j.ActorID <= 0 {
		return worker.ErrUnapproved
	}

	level, err := v.Authz.CanEdit(ctx, j.ActorID, j.DatabaseID)
	if err != nil {
		return fmt.Errorf("revalidate edit access: %w", err)
	}
	if !level {
		return ErrForbidden
	}

	var databaseID, definitionID, revisionNumber, latestRevision int64
	var revisionID int64
	var upSQL, downSQL string
	var irreversible bool
	var revisionStatus, definitionStatus WorkflowStatus
	var executedAt sql.NullTime
	err = v.Platform.QueryRowContext(ctx, `SELECT d.database_id,d.id,r.id,r.revision,r.up_sql,COALESCE(r.down_sql,''),r.irreversible,
		r.workflow_status,d.workflow_status,r.executed_at,
		(SELECT MAX(revision) FROM migration_revisions WHERE definition_id=d.id)
		FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id
		WHERE d.id=$1 AND r.revision=$2`, j.MigrationID, j.Revision).Scan(
		&databaseID, &definitionID, &revisionID, &revisionNumber, &upSQL, &downSQL, &irreversible,
		&revisionStatus, &definitionStatus, &executedAt, &latestRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return worker.ErrUnapproved
	}
	if err != nil {
		return fmt.Errorf("load approved migration snapshot: %w", err)
	}
	mode, table, size, configErr := approvedBatchConfig(upSQL)
	if configErr != nil {
		return ErrJobSnapshotMismatch
	}
	if databaseID != j.DatabaseID || definitionID != j.MigrationID || revisionNumber != j.Revision ||
		j.Mode != mode || j.BatchTable != table || j.BatchSize != size ||
		(mode == worker.Batch && !irreversible && !strings.Contains(downSQL, "$1")) ||
		j.SQL != upSQL || j.ReverseSQL != downSQL || (irreversible && j.ReverseSQL != "") {
		return ErrJobSnapshotMismatch
	}
	if (revisionStatus != WorkflowApproved && !(j.Operation != worker.Apply && revisionStatus == WorkflowExecuted)) ||
		(j.Operation == worker.Apply && (definitionStatus != WorkflowApproved || latestRevision != j.Revision)) {
		return worker.ErrUnapproved
	}

	var ready bool
	err = v.Platform.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_review_groups WHERE revision_id=$1)
		AND NOT EXISTS(SELECT 1 FROM migration_review_groups rg LEFT JOIN migration_approvals a
		 ON a.revision_id=rg.revision_id AND a.group_id=rg.group_id AND a.user_id=rg.leader_user_id AND a.decision='approved'
		 WHERE rg.revision_id=$1 AND a.id IS NULL)`, revisionID).Scan(&ready)
	if err != nil {
		return fmt.Errorf("revalidate approvals: %w", err)
	}
	if !ready {
		return worker.ErrUnapproved
	}

	var exists bool
	switch j.Operation {
	case worker.Apply:
		if executedAt.Valid {
			return ErrMigrationAlreadyExecuted
		}
		err = v.Platform.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_worker_history WHERE migration_id=$1)
			OR EXISTS(SELECT 1 FROM migration_jobs WHERE migration_id=$1 AND status='success')`, definitionID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check prior execution: %w", err)
		}
		if exists {
			return ErrMigrationAlreadyExecuted
		}
		// Every earlier definition must be active; no later definition may
		// already be active. History is append-only, so derive state per definition.
		err = v.Platform.QueryRowContext(ctx, `WITH latest AS (
			SELECT DISTINCT ON (h.migration_id) h.migration_id,h.operation
			FROM migration_worker_history h JOIN migration_definitions d ON d.id=h.migration_id
			WHERE d.database_id=$1 ORDER BY h.migration_id,h.committed_at DESC,h.execution_id DESC)
			SELECT EXISTS(SELECT 1 FROM migration_definitions d WHERE d.database_id=$1 AND d.id<$2
				AND NOT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=d.id AND l.operation IN ('apply','forward')))
			OR EXISTS(SELECT 1 FROM latest l JOIN migration_definitions d ON d.id=l.migration_id
				WHERE d.database_id=$1 AND d.id>$2 AND l.operation IN ('apply','forward'))`, databaseID, definitionID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration order: %w", err)
		}
		if exists {
			return ErrMigrationOrderInvalid
		}
	case worker.Rollback:
		if irreversible || downSQL == "" {
			return ErrIrreversibleRollback
		}
		if j.SourceExecutionID == "" {
			return ErrRollbackUnavailable
		}
		var active bool
		err = v.Platform.QueryRowContext(ctx, `WITH latest AS (
			SELECT DISTINCT ON (h.migration_id) h.migration_id,h.execution_id,h.operation,h.revision
			FROM migration_worker_history h JOIN migration_definitions d ON d.id=h.migration_id
			WHERE d.database_id=$1 ORDER BY h.migration_id,h.committed_at DESC,h.execution_id DESC)
			SELECT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=$2 AND l.execution_id=$3 AND l.revision=$4 AND l.operation IN ('apply','forward'))
			AND NOT EXISTS(SELECT 1 FROM migration_definitions earlier WHERE earlier.database_id=$1 AND earlier.id<$2
				AND NOT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=earlier.id AND l.operation IN ('apply','forward')))
			AND NOT EXISTS(SELECT 1 FROM latest l JOIN migration_definitions d ON d.id=l.migration_id
			WHERE d.database_id=$1 AND d.id>$2 AND l.operation IN ('apply','forward'))`, databaseID, definitionID, j.SourceExecutionID, j.Revision).Scan(&active)
		if err != nil {
			return fmt.Errorf("check rollback position: %w", err)
		}
		if !active {
			return ErrRollbackUnavailable
		}
	case worker.Forward:
		if j.SourceExecutionID == "" {
			return ErrForwardUnavailable
		}
		var eligible bool
		err = v.Platform.QueryRowContext(ctx, `WITH latest AS (
			SELECT DISTINCT ON (h.migration_id) h.migration_id,h.execution_id,h.operation,h.revision,h.source_execution_id
			FROM migration_worker_history h JOIN migration_definitions d ON d.id=h.migration_id
			WHERE d.database_id=$1 ORDER BY h.migration_id,h.committed_at DESC,h.execution_id DESC)
			SELECT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=$2 AND l.execution_id=$3 AND l.revision=$4 AND l.operation='rollback'
				AND EXISTS(SELECT 1 FROM migration_worker_history applied WHERE applied.execution_id=l.source_execution_id
					AND applied.migration_id=$2 AND applied.revision=$4 AND applied.operation IN ('apply','forward')))
			AND NOT EXISTS(SELECT 1 FROM migration_definitions earlier WHERE earlier.database_id=$1 AND earlier.id<$2
				AND NOT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=earlier.id AND l.operation IN ('apply','forward')))
			AND NOT EXISTS(SELECT 1 FROM latest l JOIN migration_definitions d ON d.id=l.migration_id
				WHERE d.database_id=$1 AND d.id>$2 AND l.operation IN ('apply','forward'))`, databaseID, definitionID, j.SourceExecutionID, j.Revision).Scan(&eligible)
		if err != nil {
			return fmt.Errorf("check forward position: %w", err)
		}
		if !eligible {
			return ErrForwardUnavailable
		}
	}

	var active bool
	err = v.Platform.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs j WHERE j.database_id=$1
		AND j.id<>$2 AND j.status IN ('running','partial','partially_reverted','uncertain'))`, databaseID, j.ID).Scan(&active)
	if err != nil {
		return fmt.Errorf("check database execution lock: %w", err)
	}
	if active {
		return ErrDatabaseExecutionBlocked
	}

	err = v.Platform.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs WHERE migration_id=$1 AND revision=$2
		AND id<>$3 AND status IN ('pending','running','retrying','partial','partially_reverted','uncertain'))`, definitionID, j.Revision, j.ID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check duplicate job: %w", err)
	}
	if exists {
		return ErrExecutionAlreadyQueued
	}
	return nil
}
