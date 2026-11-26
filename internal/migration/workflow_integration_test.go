package migration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/platformdb"
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
	if err != nil || status.RequiredApprovals != 2 || status.CompletedApprovals != 1 {
		t.Fatalf("review obligations changed after submission: status=%+v err=%v", status, err)
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
