package migration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestExecutionHistoryAttemptsHideRollbackAndAppendOnly(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	type record struct {
		op, execution, source, status, sql, sqlState string
		migrationID, attempt                         int64
		started                                      time.Time
	}
	base := time.Now().UTC().Add(-time.Hour)
	rows := []record{
		{op: "apply", execution: "E2", status: "failed", sql: "UPDATE items SET value = value / 0", sqlState: "22012", migrationID: 1001, attempt: 1, started: base},
		{op: "apply", execution: "E2", status: "success", sql: "UPDATE items SET value = value + 1", migrationID: 1001, attempt: 2, started: base.Add(time.Minute)},
		{op: "rollback", execution: "E3", source: "E2", status: "success", sql: "UPDATE items SET value = value - 1", migrationID: 1001, attempt: 1, started: base.Add(2 * time.Minute)},
		{op: "forward", execution: "E4", source: "E3", status: "success", sql: "UPDATE items SET value = value + 1", migrationID: 1001, attempt: 1, started: base.Add(3 * time.Minute)},
		{op: "rollback", execution: "E5", source: "E4", status: "failed", sql: "UPDATE items SET value = value - 1", sqlState: "22012", migrationID: 2002, attempt: 1, started: base.Add(4 * time.Minute)},
	}
	jobIDs := map[string]string{}
	for i, item := range rows {
		jobID, exists := jobIDs[item.execution]
		if !exists {
			jobID = fmt.Sprintf("history-job-%d-%d", time.Now().UnixNano(), i)
			jobIDs[item.execution] = jobID
			if _, err := f.db.ExecContext(ctx, `INSERT INTO migration_jobs
				(id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,source_execution_id,status)
				VALUES($1,$2,$3,$4,1,$5,$6,'normal',$7,NULLIF($8,''),$9)`, jobID, item.execution, f.databaseID, item.migrationID, f.user1,
				item.op, item.sql, item.source, item.status); err != nil {
				t.Fatal(err)
			}
		}
		var ended any = item.started.Add(time.Second)
		if _, err := f.db.ExecContext(ctx, `INSERT INTO migration_job_attempts
			(job_id,attempt,started_at,ended_at,outcome,error_text,execution_id,migration_id,revision,actor_id,operation,sql_text,sqlstate,sql_started)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,1,$9,$10,$11,NULLIF($12,''),TRUE)`, jobID, item.attempt, item.started, ended,
			item.status, func() string {
				if item.status == "failed" {
					return "test SQL failure"
				}
				return ""
			}(), item.execution, item.migrationID, f.user1, item.op, item.sql, item.sqlState); err != nil {
			t.Fatal(err)
		}
	}

	all, total, err := f.svc.ListExecutionHistory(ctx, f.user1, f.databaseID, 1, 2, false)
	if err != nil || total != 5 || len(all) != 2 {
		t.Fatalf("history page 1 total=%d rows=%d err=%v", total, len(all), err)
	}
	if all[0].ExecutionID != "E5" || all[0].SQLState != "22012" || all[0].SQL != rows[4].sql {
		t.Fatalf("latest failed rollback SQL/error not recorded: %+v", all[0])
	}
	page2, total, err := f.svc.ListExecutionHistory(ctx, f.user1, f.databaseID, 2, 2, false)
	if err != nil || total != 5 || len(page2) != 2 || page2[0].ExecutionID != "E3" || page2[1].ExecutionID != "E2" {
		t.Fatalf("history pagination/sort page 2=%+v total=%d err=%v", page2, total, err)
	}
	page3, total, err := f.svc.ListExecutionHistory(ctx, f.user1, f.databaseID, 3, 2, false)
	if err != nil || total != 5 || len(page3) != 1 || page3[0].ExecutionID != "E2" || page3[0].Status != "failed" || page3[0].SQLState != "22012" {
		t.Fatalf("failed retry attempt was not paginated and retained: %+v total=%d err=%v", page3, total, err)
	}
	hidden, hiddenTotal, err := f.svc.ListExecutionHistory(ctx, f.user1, f.databaseID, 1, 20, true)
	if err != nil || hiddenTotal != 3 {
		t.Fatalf("Hide rollback total=%d err=%v", hiddenTotal, err)
	}
	seen := map[string]bool{}
	for _, item := range hidden {
		seen[item.ExecutionID+":"+item.Status] = true
	}
	if len(hidden) != 3 || !seen["E2:failed"] || !seen["E4:success"] || !seen["E5:failed"] || seen["E2:success"] || seen["E3:success"] {
		t.Fatalf("Hide rollback removed the wrong attempts: %+v", hidden)
	}
	if hidden[0].SourceExecutionID != "E4" || hidden[0].Operation != "rollback" {
		t.Fatalf("failed rollback source relation missing: %+v", hidden[0])
	}

	var attemptJobID string
	if err := f.db.QueryRowContext(ctx, `SELECT job_id FROM migration_job_attempts WHERE execution_id='E2' AND attempt=2`).Scan(&attemptJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE migration_job_attempts SET sql_text='SELECT 1' WHERE job_id=$1 AND attempt=2`, attemptJobID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("completed attempt SQL should be immutable, got %v", err)
	}
	if _, err := f.db.ExecContext(ctx, `DELETE FROM migration_job_attempts WHERE job_id=$1 AND attempt=2`, attemptJobID); err == nil || !strings.Contains(err.Error(), "job attempts are append-only") {
		t.Fatalf("attempt deletion should be rejected, got %v", err)
	}
	denied := NewWorkflowService(f.db, fakeAuthz{view: false})
	if _, _, err := denied.ListExecutionHistory(ctx, f.user1, f.databaseID, 1, 20, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("history authorization should deny users without access, got %v", err)
	}
}
