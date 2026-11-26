package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type OperationAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type ActiveMigrationJob struct {
	JobID       string `json:"job_id"`
	ExecutionID string `json:"execution_id"`
	Operation   string `json:"operation"`
	Status      string `json:"status"`
}

type MigrationOperations struct {
	LastOperation   string                `json:"last_operation,omitempty"`
	LastExecutionID string                `json:"last_execution_id,omitempty"`
	Revision        int64                 `json:"revision,omitempty"`
	ActiveJob       *ActiveMigrationJob   `json:"active_job,omitempty"`
	Apply           OperationAvailability `json:"apply"`
	Rollback        OperationAvailability `json:"rollback"`
	Forward         OperationAvailability `json:"forward"`
}

func (s *WorkflowService) ExecutionOperations(ctx context.Context, userID, definitionID int64) (MigrationOperations, error) {
	var out MigrationOperations
	if definitionID <= 0 || userID <= 0 {
		return out, ErrInvalidArgument
	}
	definition, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return out, err
	}
	if err = s.canEdit(ctx, userID, definition.DatabaseID); err != nil {
		return out, err
	}

	var irreversible bool
	var downSQL string
	var sourceExecutionID string
	hasHistory := true
	err = s.Repo.DB.QueryRowContext(ctx, `SELECT h.operation,h.execution_id,h.revision,COALESCE(h.source_execution_id,''),r.irreversible,COALESCE(r.down_sql,'')
		FROM migration_worker_history h
		JOIN migration_revisions r ON r.definition_id=h.migration_id AND r.revision=h.revision
		WHERE h.migration_id=$1 ORDER BY h.committed_at DESC,h.execution_id DESC LIMIT 1`, definitionID).
		Scan(&out.LastOperation, &out.LastExecutionID, &out.Revision, &sourceExecutionID, &irreversible, &downSQL)
	if errors.Is(err, sql.ErrNoRows) {
		hasHistory = false
		out.Rollback.Reason = "Migration ini belum memiliki eksekusi sukses."
		out.Forward.Reason = "Belum ada rollback sukses untuk di-forward."
	} else if err != nil {
		return out, fmt.Errorf("load latest migration execution: %w", err)
	}

	var activeJob ActiveMigrationJob
	err = s.Repo.DB.QueryRowContext(ctx, `SELECT id,execution_id,operation,status FROM migration_jobs WHERE migration_id=$1
		AND status IN ('pending','running','retrying','partial','partially_reverted','uncertain')
		ORDER BY created_at DESC,id DESC LIMIT 1`, definitionID).Scan(&activeJob.JobID, &activeJob.ExecutionID, &activeJob.Operation, &activeJob.Status)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err != nil {
		return out, fmt.Errorf("check migration jobs: %w", err)
	} else {
		out.ActiveJob = &activeJob
	}
	migrationJobActive := out.ActiveJob != nil
	var databaseBlocked, successfulJob bool
	if err = s.Repo.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs WHERE database_id=$1
		AND status IN ('running','partial','partially_reverted','uncertain'))`, definition.DatabaseID).Scan(&databaseBlocked); err != nil {
		return out, fmt.Errorf("check database execution state: %w", err)
	}
	if err = s.Repo.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs WHERE migration_id=$1 AND status='success')`, definitionID).Scan(&successfulJob); err != nil {
		return out, fmt.Errorf("check prior successful jobs: %w", err)
	}

	var earlierInactive, laterActive bool
	if err = s.Repo.DB.QueryRowContext(ctx, `WITH latest AS (
		SELECT DISTINCT ON (h.migration_id) h.migration_id,h.operation
		FROM migration_worker_history h JOIN migration_definitions d ON d.id=h.migration_id
		WHERE d.database_id=$1 ORDER BY h.migration_id,h.committed_at DESC,h.execution_id DESC)
		SELECT EXISTS(SELECT 1 FROM migration_definitions d WHERE d.database_id=$1 AND d.id<$2
			AND NOT EXISTS(SELECT 1 FROM latest l WHERE l.migration_id=d.id AND l.operation IN ('apply','forward'))),
		EXISTS(SELECT 1 FROM latest l JOIN migration_definitions d ON d.id=l.migration_id
			WHERE d.database_id=$1 AND d.id>$2 AND l.operation IN ('apply','forward'))`,
		definition.DatabaseID, definitionID).Scan(&earlierInactive, &laterActive); err != nil {
		return out, fmt.Errorf("check migration order: %w", err)
	}

	orderValid := !earlierInactive && !laterActive
	switch {
	case migrationJobActive:
		out.Apply.Reason = "Migration ini masih memiliki job aktif."
		out.Rollback.Reason = "Migration ini masih memiliki job aktif."
		out.Forward.Reason = "Migration ini masih memiliki job aktif."
	case databaseBlocked:
		out.Apply.Reason = "Database sedang diblokir oleh eksekusi yang belum aman."
		out.Rollback.Reason = "Database sedang diblokir oleh eksekusi yang belum aman."
		out.Forward.Reason = "Database sedang diblokir oleh eksekusi yang belum aman."
	case !orderValid:
		out.Apply.Reason = "Urutan migration sebelumnya belum terpenuhi atau migration berikutnya sudah aktif."
		out.Rollback.Reason = "Migration ini bukan posisi terakhir yang aktif sesuai urutan."
		out.Forward.Reason = "Migration ini bukan posisi terakhir yang aktif sesuai urutan."
	default:
		switch {
		case hasHistory:
			out.Apply.Reason = "Migration sudah memiliki riwayat eksekusi; gunakan rollback atau forward."
		case successfulJob:
			out.Apply.Reason = "Migration sudah memiliki job sukses."
		default:
			out.Apply.Available = true
		}

		if hasHistory && (out.LastOperation == "apply" || out.LastOperation == "forward") {
			switch {
			case irreversible || downSQL == "":
				out.Rollback.Reason = "Migration irreversible atau tidak memiliki Down SQL."
			default:
				out.Rollback.Available = true
			}
		} else {
			out.Rollback.Reason = "State terakhir bukan eksekusi aktif yang dapat di-rollback."
		}

		if hasHistory && out.LastOperation == "rollback" && sourceExecutionID != "" {
			var sourceExists bool
			err = s.Repo.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_worker_history
				WHERE execution_id=$1 AND migration_id=$2 AND revision=$3 AND operation IN ('apply','forward'))`,
				sourceExecutionID, definitionID, out.Revision).Scan(&sourceExists)
			if err != nil {
				return out, fmt.Errorf("validate forward source execution: %w", err)
			}
			if sourceExists {
				out.Forward.Available = true
			} else {
				out.Forward.Reason = "Riwayat sumber rollback tidak cocok dengan revision."
			}
		} else {
			out.Forward.Reason = "State terakhir bukan rollback sukses yang dapat di-forward."
		}
	}

	return out, nil
}
