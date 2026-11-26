package migration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/platformdb"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type workflowFixture struct {
	db                                                  *sql.DB
	svc                                                 *WorkflowService
	user1, user2, nonLeader, databaseID, group1, group2 int64
}

func workflowSetup(t *testing.T, includeSecondGroup bool) workflowFixture {
	t.Helper()
	dsn := os.Getenv("TEST_PLATFORM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_PLATFORM_DATABASE_URL not set")
	}
	db := openTestDB(t, dsn)
	if err := platformdb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("platform migrate: %v", err)
	}
	insertUser := func(name string) int64 {
		var id int64
		if err := db.QueryRow(`INSERT INTO users(username,password_hash) VALUES($1,'x') RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	u1, u2, outsider := insertUser("workflow_leader_a"), insertUser("workflow_leader_b"), insertUser("workflow_outsider")
	var dbID int64
	if err := db.QueryRow(`INSERT INTO database_connections(name) VALUES('workflow_target') RETURNING id`).Scan(&dbID); err != nil {
		t.Fatal(err)
	}
	var g1, g2 int64
	if err := db.QueryRow(`INSERT INTO app_groups(name,leader_user_id) VALUES('workflow_group_a',$1) RETURNING id`, u1).Scan(&g1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)`, u1, g1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO group_database_access(group_id,database_id,access_level) VALUES($1,$2,'VIEW_EDIT')`, g1, dbID); err != nil {
		t.Fatal(err)
	}
	if includeSecondGroup {
		if err := db.QueryRow(`INSERT INTO app_groups(name,leader_user_id) VALUES('workflow_group_b',$1) RETURNING id`, u2).Scan(&g2); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)`, u2, g2); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO group_database_access(group_id,database_id,access_level) VALUES($1,$2,'VIEW_ONLY')`, g2, dbID); err != nil {
			t.Fatal(err)
		}
	}
	return workflowFixture{db: db, svc: NewWorkflowService(db, fakeAuthz{view: true, edit: true}), user1: u1, user2: u2, nonLeader: outsider, databaseID: dbID, group1: g1, group2: g2}
}

func validRevision(name string) RevisionInput {
	return RevisionInput{Name: name, UpSQL: "CREATE TABLE sample(id BIGINT)", DownSQL: "DROP TABLE sample"}
}

func approveMigration(t *testing.T, f workflowFixture, name string, irreversible bool) (MigrationDefinition, MigrationRevision) {
	t.Helper()
	in := validRevision(name)
	if irreversible {
		in.Irreversible, in.DownSQL, in.IrreversibleReason = true, "", "cannot be reversed safely"
	}
	d, revision, err := f.svc.CreateMigration(context.Background(), f.user1, f.databaseID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(context.Background(), f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.ReviewStatus(context.Background(), f.user1, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range status.RequiredGroups {
		leader := group.LeaderUserID
		if _, _, err = f.svc.Approve(context.Background(), leader, revision.ID, group.GroupID); err != nil {
			t.Fatal(err)
		}
	}
	return d, revision
}

func recordWorkerHistory(t *testing.T, f workflowFixture, d MigrationDefinition, revision MigrationRevision, operation worker.Operation, executionID, sourceExecutionID string) {
	t.Helper()
	jobID := "job-" + executionID
	_, err := f.db.Exec(`INSERT INTO migration_jobs(id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,reverse_sql,source_execution_id,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,'normal',$8,$9,NULLIF($10,''),'success')`, jobID, executionID, f.databaseID, d.ID, revision.Revision, f.user1, operation, revision.UpSQL, nullableTestSQL(revision.DownSQL), sourceExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	statement := revision.UpSQL
	if operation == worker.Rollback {
		statement = revision.DownSQL
	}
	if _, err = f.db.Exec(`INSERT INTO migration_worker_history(execution_id,job_id,database_id,migration_id,revision,actor_id,operation,source_execution_id,sql_text)
		VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`, executionID, jobID, f.databaseID, d.ID, revision.Revision, f.user1, operation, sourceExecutionID, statement); err != nil {
		t.Fatal(err)
	}
}

func nullableTestSQL(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestWorkflowCreateAndUpdateMigration(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, first, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("first"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.WorkflowStatus != WorkflowDraft || d.ID <= 0 {
		t.Fatalf("definition=%+v revision=%+v", d, first)
	}
	in := validRevision("renamed")
	in.Description = "updated metadata"
	in.UpSQL = "CREATE TABLE sample(id BIGINT, value TEXT)"
	second, err := f.svc.UpdateMigration(ctx, f.user1, d.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.UpSQL != in.UpSQL {
		t.Fatalf("revision=%+v", second)
	}
	var count int
	if err = f.db.QueryRow(`SELECT count(*) FROM migration_revisions WHERE definition_id=$1`, d.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("revision count=%d err=%v", count, err)
	}
}

func TestWorkflowIrreversibleRequiresReason(t *testing.T) {
	f := workflowSetup(t, false)
	in := validRevision("irreversible")
	in.Irreversible = true
	in.DownSQL = ""
	in.IrreversibleReason = "  "
	if _, _, err := f.svc.CreateMigration(context.Background(), f.user1, f.databaseID, in); !errors.Is(err, ErrIrreversibleReason) {
		t.Fatalf("want ErrIrreversibleReason, got %v", err)
	}
}

func TestWorkflowSubmittedRevisionIsImmutable(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("immutable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.UpdateMigration(ctx, f.user1, d.ID, validRevision("must not replace submitted")); !errors.Is(err, ErrRevisionImmutable) {
		t.Fatalf("want ErrRevisionImmutable while review is pending, got %v", err)
	}
	if _, err = f.db.Exec(`UPDATE migration_revisions SET up_sql='DROP TABLE users' WHERE id=$1`, v.ID); err == nil {
		t.Fatal("submitted revision SQL update unexpectedly succeeded")
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, v.ID, f.group1); err != nil {
		t.Fatal(err)
	}
	if executed, markErr := f.svc.MarkExecuted(ctx, d.ID, v.ID, "execution-immutable"); markErr != nil || executed.WorkflowStatus != WorkflowExecuted {
		t.Fatalf("executed=%+v err=%v", executed, markErr)
	}
	if _, err = f.db.Exec(`UPDATE migration_revisions SET down_sql='SELECT 1' WHERE id=$1`, v.ID); err == nil {
		t.Fatal("executed revision SQL update unexpectedly succeeded")
	}
}

func TestWorkflowReviewPartialAndCompleteApproval(t *testing.T) {
	f := workflowSetup(t, true)
	ctx := context.Background()
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("approvals"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)`, f.nonLeader, f.group2); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.SetGroupLeader(ctx, f.group2, f.nonLeader); err != nil {
		t.Fatalf("reassign current group leader: %v", err)
	}
	_, complete, err := f.svc.Approve(ctx, f.user1, v.ID, f.group1)
	if err != nil || complete {
		t.Fatalf("partial approval complete=%v err=%v", complete, err)
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, v.ID, f.group1); !errors.Is(err, ErrApprovalDuplicate) {
		t.Fatalf("want duplicate approval error, got %v", err)
	}
	if _, err = f.svc.ApprovedRevision(ctx, d.ID); !errors.Is(err, ErrApprovalIncomplete) {
		t.Fatalf("want incomplete, got %v", err)
	}
	if _, err = f.db.Exec(`DELETE FROM group_database_access WHERE group_id=$1 AND database_id=$2`, f.group2, f.databaseID); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.ReviewStatus(ctx, f.user1, d.ID)
	if err != nil || status.RequiredApprovals != 2 || status.CompletedApprovals != 1 || len(status.RequiredGroups) != 2 {
		t.Fatalf("review obligations changed after submission: status=%+v err=%v", status, err)
	}
	snapshots := map[int64]ReviewGroup{}
	for _, group := range status.RequiredGroups {
		snapshots[group.GroupID] = group
	}
	if snapshots[f.group1].LeaderUserID != f.user1 || snapshots[f.group2].LeaderUserID != f.user2 ||
		snapshots[f.group1].Decision != "approved" || snapshots[f.group2].Decision != "pending" {
		t.Fatalf("review group snapshot changed or is incomplete: %+v", status.RequiredGroups)
	}
	_, complete, err = f.svc.Approve(ctx, f.user2, v.ID, f.group2)
	if err != nil || !complete {
		t.Fatalf("complete approval complete=%v err=%v", complete, err)
	}
	approved, err := f.svc.ApprovedRevision(ctx, d.ID)
	if err != nil || approved.WorkflowStatus != WorkflowApproved {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	var count int
	if err = f.db.QueryRow(`SELECT count(*) FROM migration_approvals WHERE revision_id=$1`, v.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("approval count=%d err=%v", count, err)
	}
}

func TestWorkflowApprovalRejectsNonLeader(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("leader"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.Approve(ctx, f.nonLeader, v.ID, f.group1); !errors.Is(err, ErrNotGroupLeader) {
		t.Fatalf("want ErrNotGroupLeader, got %v", err)
	}
}

func TestWorkflowRejectRecordsLeaderAndMakesRevisionTerminal(t *testing.T) {
	f := workflowSetup(t, true)
	ctx := context.Background()
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("reject me"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	decision, err := f.svc.RejectForDefinition(ctx, f.user2, d.ID, v.Revision, f.group2)
	if err != nil || decision.Decision != "rejected" || decision.UserID != f.user2 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	status, err := f.svc.ReviewStatus(ctx, f.user1, d.ID)
	if err != nil || status.Status != WorkflowRejected || status.RejectedApprovals != 1 || status.Ready {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, v.ID, f.group1); !errors.Is(err, ErrWorkflowInvalid) {
		t.Fatalf("want terminal workflow error, got %v", err)
	}
}

func TestSubmitJobPublishesApprovedSQLSnapshot(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("submit"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitJob(ctx, f.user1, d.ID); !errors.Is(err, ErrApprovalIncomplete) {
		t.Fatalf("unapproved migration was submitted: %v", err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, v.ID, f.group1); err != nil {
		t.Fatal(err)
	}
	failedJob, enqueueErr := f.svc.SubmitJob(ctx, 999999, d.ID)
	if enqueueErr == nil || failedJob.ID != "" || failedJob.ExecutionID != "" {
		t.Fatalf("enqueue failure returned successful job: job=%+v err=%v", failedJob, enqueueErr)
	}
	var queuedCount int
	if err = f.db.QueryRow(`SELECT count(*) FROM migration_jobs WHERE migration_id=$1`, d.ID).Scan(&queuedCount); err != nil || queuedCount != 0 {
		t.Fatalf("failed enqueue persisted jobs=%d err=%v", queuedCount, err)
	}
	job, err := f.svc.SubmitJob(ctx, f.user1, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || len(job.ExecutionID) != 32 || job.Status != worker.Pending || job.SQL != v.UpSQL || job.ReverseSQL != v.DownSQL {
		t.Fatalf("job snapshot=%+v", job)
	}
	var sqlText, reverseSQL string
	if err = f.db.QueryRow(`SELECT sql_text,reverse_sql FROM migration_jobs WHERE id=$1`, job.ID).Scan(&sqlText, &reverseSQL); err != nil || sqlText != v.UpSQL || reverseSQL != v.DownSQL {
		t.Fatalf("persisted SQL=%q reverse=%q err=%v", sqlText, reverseSQL, err)
	}
	if _, err = f.svc.SubmitJob(ctx, f.user1, d.ID); !errors.Is(err, ErrExecutionAlreadyQueued) {
		t.Fatalf("want duplicate job rejection, got %v", err)
	}
	changed := job
	changed.ID = "different-job"
	changed.SQL += "; SELECT 1"
	if err = f.svc.Revalidator.Revalidate(ctx, changed); !errors.Is(err, ErrJobSnapshotMismatch) {
		t.Fatalf("want snapshot mismatch, got %v", err)
	}
	if _, err = f.db.Exec(`UPDATE migration_jobs SET status='running' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	changed = job
	changed.ID = "different-job"
	if err = f.svc.Revalidator.Revalidate(ctx, changed); !errors.Is(err, ErrDatabaseExecutionBlocked) {
		t.Fatalf("want blocked database error, got %v", err)
	}
	if _, err = f.db.Exec(`UPDATE migration_jobs SET status='success' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.Revalidator.Revalidate(ctx, job); !errors.Is(err, ErrMigrationAlreadyExecuted) {
		t.Fatalf("want already executed error, got %v", err)
	}
}

func TestSubmitJobRejectsOutOfOrderMigration(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	if _, _, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("first pending")); err != nil {
		t.Fatal(err)
	}
	d, v, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, v.ID, f.group1); err != nil {
		t.Fatal(err)
	}
	operations, err := f.svc.ExecutionOperations(ctx, f.user1, d.ID)
	if err != nil || operations.Apply.Available || operations.Apply.Reason == "" {
		t.Fatalf("out-of-order migration should not be advertised as executable: operations=%+v err=%v", operations, err)
	}
	if _, err = f.svc.SubmitJob(ctx, f.user1, d.ID); !errors.Is(err, ErrMigrationOrderInvalid) {
		t.Fatalf("want migration order error, got %v", err)
	}
}

func TestRollbackAndForwardJobsUseApprovedImmutableSnapshot(t *testing.T) {
	f := workflowSetup(t, false)
	d, revision := approveMigration(t, f, "rollback-forward", false)
	recordWorkerHistory(t, f, d, revision, worker.Apply, "apply-original", "")
	operations, err := f.svc.ExecutionOperations(context.Background(), f.user1, d.ID)
	if err != nil || operations.Apply.Available || !operations.Rollback.Available || operations.Forward.Available {
		t.Fatalf("operations after apply=%+v err=%v", operations, err)
	}

	rollback, rollbackReview := approveOperationReview(t, f, d.ID, worker.Rollback)
	operations, err = f.svc.ExecutionOperations(context.Background(), f.user1, d.ID)
	if err != nil || operations.ActiveJob == nil || operations.ActiveJob.JobID != rollback.ID || operations.Rollback.Available {
		t.Fatalf("queued operation should be exposed and suppress duplicates: operations=%+v err=%v", operations, err)
	}
	if rollback.Operation != worker.Rollback || rollback.SQL != revision.UpSQL || rollback.ReverseSQL != revision.DownSQL || rollback.SourceExecutionID != "apply-original" || rollback.ExecutionID == "apply-original" || rollback.OperationReviewID != rollbackReview.ID {
		t.Fatalf("rollback snapshot/source is wrong: %+v", rollback)
	}
	var storedSource string
	if err = f.db.QueryRow(`SELECT source_execution_id FROM migration_jobs WHERE id=$1`, rollback.ID).Scan(&storedSource); err != nil || storedSource != "apply-original" {
		t.Fatalf("rollback source was not persisted: source=%q err=%v", storedSource, err)
	}

	recordWorkerHistory(t, f, d, revision, worker.Rollback, "rollback-done", "apply-original")
	// Simulate completion of the queued rollback job.
	_, err = f.db.Exec(`UPDATE migration_jobs SET status = 'success', finished_at = NOW() WHERE id = $1`, rollback.ID)
	if err != nil {
		t.Fatal(err)
	}
	operations, err = f.svc.ExecutionOperations(context.Background(), f.user1, d.ID)
	if err != nil || operations.Apply.Available || !operations.Forward.Available || operations.Rollback.Available {
		t.Fatalf("operations after rollback=%+v err=%v", operations, err)
	}
	forward, forwardReview := approveOperationReview(t, f, d.ID, worker.Forward)
	if forward.Operation != worker.Forward || forward.SQL != revision.UpSQL || forward.ReverseSQL != revision.DownSQL || forward.SourceExecutionID != "rollback-done" || forward.ExecutionID == "rollback-done" {
		t.Fatalf("forward snapshot/source is wrong: %+v", forward)
	}
	if forward.OperationReviewID != forwardReview.ID {
		t.Fatalf("forward did not bind its new approval review: %+v", forward)
	}
}

func approveOperationReview(t *testing.T, f workflowFixture, definitionID int64, operation worker.Operation) (worker.Job, OperationReview) {
	t.Helper()
	ctx := context.Background()
	review, err := f.svc.StartOperationReview(ctx, f.user1, definitionID, operation)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range review.Groups {
		review, err = f.svc.DecideOperationReview(ctx, group.LeaderUserID, review.ID, group.GroupID, "approved")
		if err != nil {
			t.Fatal(err)
		}
	}
	if review.Status != "approved" {
		t.Fatalf("operation review is not approved: %+v", review)
	}
	job, err := f.svc.SubmitOperationReview(ctx, f.user1, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job, review
}

func TestOperationReviewRequiresEveryCurrentGroupAndRevalidatesPermission(t *testing.T) {
	f := workflowSetup(t, true)
	ctx := context.Background()
	d, revision := approveMigration(t, f, "multi-group-operation", false)
	recordWorkerHistory(t, f, d, revision, worker.Apply, "multi-group-source", "")
	review, err := f.svc.StartOperationReview(ctx, f.user1, d.ID, worker.Rollback)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Groups) != 2 {
		t.Fatalf("review did not snapshot both access groups: %+v", review.Groups)
	}
	if _, err = f.svc.DecideOperationReview(ctx, f.user1, review.ID, f.group1, "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitOperationReview(ctx, f.user1, review.ID); !errors.Is(err, ErrApprovalIncomplete) {
		t.Fatalf("one group approval must be insufficient: %v", err)
	}
	if _, err = f.svc.DecideOperationReview(ctx, f.user2, review.ID, f.group2, "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DELETE FROM group_database_access WHERE group_id=$1 AND database_id=$2`, f.group2, f.databaseID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitOperationReview(ctx, f.user1, review.ID); !errors.Is(err, worker.ErrUnapproved) {
		t.Fatalf("permission changes before enqueue must invalidate approval: %v", err)
	}
}

func TestOperationReviewRejectionPreventsSubmission(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, revision := approveMigration(t, f, "rejected-operation", false)
	recordWorkerHistory(t, f, d, revision, worker.Apply, "rejected-source", "")
	review, err := f.svc.StartOperationReview(ctx, f.user1, d.ID, worker.Rollback)
	if err != nil {
		t.Fatal(err)
	}
	review, err = f.svc.DecideOperationReview(ctx, f.user1, review.ID, f.group1, "rejected")
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != "rejected" {
		t.Fatalf("review status=%s", review.Status)
	}
	if _, err = f.svc.SubmitOperationReview(ctx, f.user1, review.ID); !errors.Is(err, ErrApprovalIncomplete) {
		t.Fatalf("rejected operation must not queue: %v", err)
	}
}

func TestPartialJobRequiresExplicitResume(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	in := RevisionInput{Name: "partial-resume", UpSQL: "-- dbmp:batch table=items size=10\nUPDATE items SET done=TRUE WHERE id=ANY($1::bigint[])", DownSQL: "UPDATE items SET done=FALSE WHERE id=ANY($1::bigint[])"}
	d, revision, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.svc.Approve(ctx, f.user1, revision.ID, f.group1); err != nil {
		t.Fatal(err)
	}
	job := worker.Job{ID: "partial-resume-job", ExecutionID: "partial-resume-exec", DatabaseID: f.databaseID, MigrationID: d.ID, Revision: revision.Revision, ActorID: f.user1, Operation: worker.Apply, Mode: worker.Batch, SQL: revision.UpSQL, ReverseSQL: revision.DownSQL, BatchTable: "items", BatchSize: 10, MaxAttempts: 4, Attempt: 4}
	_, err = f.db.Exec(`INSERT INTO migration_jobs(id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,reverse_sql,batch_table,batch_size,max_attempts,attempt,batches_done,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,1,'partial')`, job.ID, job.ExecutionID, job.DatabaseID, job.MigrationID, job.Revision, job.ActorID, job.Operation, job.Mode, job.SQL, job.ReverseSQL, job.BatchTable, job.BatchSize, job.MaxAttempts, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := f.svc.ResumeJob(ctx, f.user1, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "retrying" || resumed.Attempt != 4 || resumed.CanResume {
		t.Fatalf("unexpected resumed state: %+v", resumed)
	}
	var maxAttempts int
	if err = f.db.QueryRow(`SELECT max_attempts FROM migration_jobs WHERE id=$1`, job.ID).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts != 8 {
		t.Fatalf("manual resume should extend bounded attempts to 8, got %d", maxAttempts)
	}
}

func TestUnrelatedDatabaseHistoryDoesNotMarkDefinitionExecuted(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	d, _ := approveMigration(t, f, "cross-database-history", false)
	var otherDatabaseID int64
	if err := f.db.QueryRow(`INSERT INTO database_connections(name) VALUES('unrelated_history_target') RETURNING id`).Scan(&otherDatabaseID); err != nil {
		t.Fatal(err)
	}
	jobID := "unrelated-history-job"
	executionID := "unrelated-history-execution"
	if _, err := f.db.Exec(`INSERT INTO migration_jobs(id,execution_id,database_id,migration_id,revision,actor_id,operation,mode,sql_text,status)
		VALUES($1,$2,$3,$4,1,$5,'apply','normal','SELECT 1','success')`, jobID, executionID, otherDatabaseID, d.ID, f.user1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO migration_worker_history(execution_id,job_id,database_id,migration_id,revision,actor_id,operation,sql_text)
		VALUES($1,$2,$3,$4,1,$5,'apply','SELECT 1')`, executionID, jobID, otherDatabaseID, d.ID, f.user1); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.ReviewStatus(ctx, f.user1, d.ID)
	if err != nil || !status.Ready || status.Status != WorkflowApproved {
		t.Fatalf("history from another database must not change definition readiness: status=%+v err=%v", status, err)
	}
	if _, err = f.svc.ApprovedRevision(ctx, d.ID); err != nil {
		t.Fatalf("unrelated history must not block approved revision: %v", err)
	}
}

func TestRollbackRejectsMigrationThatIsNotLastActive(t *testing.T) {
	f := workflowSetup(t, false)
	first, firstRevision := approveMigration(t, f, "first-active", false)
	second, secondRevision := approveMigration(t, f, "second-active", false)
	recordWorkerHistory(t, f, first, firstRevision, worker.Apply, "first-apply", "")
	recordWorkerHistory(t, f, second, secondRevision, worker.Apply, "second-apply", "")
	if _, err := f.svc.StartOperationReview(context.Background(), f.user1, first.ID, worker.Rollback); !errors.Is(err, ErrWorkflowInvalid) {
		t.Fatalf("want rollback review rejected for non-last migration, got %v", err)
	}
}

func TestRollbackRejectsIrreversibleMigration(t *testing.T) {
	f := workflowSetup(t, false)
	d, revision := approveMigration(t, f, "irreversible-active", true)
	recordWorkerHistory(t, f, d, revision, worker.Apply, "irreversible-apply", "")
	if _, err := f.svc.StartOperationReview(context.Background(), f.user1, d.ID, worker.Rollback); !errors.Is(err, ErrWorkflowInvalid) {
		t.Fatalf("want irreversible rollback review rejection, got %v", err)
	}
	operations, err := f.svc.ExecutionOperations(context.Background(), f.user1, d.ID)
	if err != nil || operations.Rollback.Available || operations.Rollback.Reason == "" {
		t.Fatalf("irreversible migration should not advertise rollback: operations=%+v err=%v", operations, err)
	}
}

func TestRollbackRequiresNewOperationApproval(t *testing.T) {
	f := workflowSetup(t, false)
	d, revision := approveMigration(t, f, "approval-required-for-rollback", false)
	recordWorkerHistory(t, f, d, revision, worker.Apply, "approval-required-apply", "")
	if _, err := f.db.Exec(`DELETE FROM migration_approvals WHERE revision_id=$1`, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitRollbackJob(context.Background(), f.user1, d.ID); !errors.Is(err, ErrApprovalIncomplete) {
		t.Fatalf("legacy direct rollback endpoint must fail closed, got %v", err)
	}
	job, review := approveOperationReview(t, f, d.ID, worker.Rollback)
	if job.OperationReviewID != review.ID {
		t.Fatal("rollback should bind a fresh operation approval")
	}
}

func TestForwardRequiresSuccessfulRollbackAndApproval(t *testing.T) {
	f := workflowSetup(t, false)
	d, revision := approveMigration(t, f, "forward-state", false)
	if _, err := f.svc.StartOperationReview(context.Background(), f.user1, d.ID, worker.Forward); !errors.Is(err, ErrWorkflowInvalid) {
		t.Fatalf("forward without execution history should fail, got %v", err)
	}
	recordWorkerHistory(t, f, d, revision, worker.Apply, "forward-apply", "")
	if _, err := f.svc.StartOperationReview(context.Background(), f.user1, d.ID, worker.Forward); !errors.Is(err, ErrWorkflowInvalid) {
		t.Fatalf("forward without rollback should fail, got %v", err)
	}
	if _, err := f.db.Exec(`UPDATE migration_revisions SET workflow_status='draft',submitted_at=NULL WHERE id=$1`, revision.ID); err == nil {
		t.Fatal("approved revision workflow was unexpectedly mutable")
	}
}

func TestWorkflowSubmitRejectsGroupWithoutLeader(t *testing.T) {
	f := workflowSetup(t, false)
	ctx := context.Background()
	var member int64
	if err := f.db.QueryRow(`INSERT INTO users(username,password_hash) VALUES('unled_member','x') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	var groupID int64
	if err := f.db.QueryRow(`INSERT INTO app_groups(name) VALUES('unled_group') RETURNING id`).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)`, member, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO group_database_access(group_id,database_id,access_level) VALUES($1,$2,'VIEW_ONLY')`, groupID, f.databaseID); err != nil {
		t.Fatal(err)
	}
	d, _, err := f.svc.CreateMigration(ctx, f.user1, f.databaseID, validRevision("missing leader"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SubmitReview(ctx, f.user1, d.ID); !errors.Is(err, ErrGroupLeaderMissing) {
		t.Fatalf("want ErrGroupLeaderMissing, got %v", err)
	}
}

func TestWorkflowSetGroupLeaderRequiresMembership(t *testing.T) {
	f := workflowSetup(t, false)
	if err := f.svc.SetGroupLeader(context.Background(), f.group1, f.nonLeader); !errors.Is(err, ErrLeaderMustBeMember) {
		t.Fatalf("want ErrLeaderMustBeMember, got %v", err)
	}
}
