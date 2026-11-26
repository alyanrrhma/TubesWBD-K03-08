package migration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type WorkflowStatus string

const (
	WorkflowDraft    WorkflowStatus = "draft"
	WorkflowInReview WorkflowStatus = "in_review"
	WorkflowApproved WorkflowStatus = "approved"
	WorkflowRejected WorkflowStatus = "rejected"
	WorkflowExecuted WorkflowStatus = "executed"
)

var (
	ErrWorkflowNotFound   = errors.New("migration workflow not found")
	ErrWorkflowInvalid    = errors.New("invalid migration workflow operation")
	ErrInvalidRevision    = errors.New("invalid migration revision input")
	ErrIrreversibleReason = errors.New("irreversible migration requires a reason")
	ErrRevisionImmutable  = errors.New("submitted or executed revision is immutable")
	ErrApprovalIncomplete = errors.New("migration revision does not have all required approvals")
	ErrNotGroupLeader     = errors.New("user is not the leader of this database access group")
	ErrGroupLeaderMissing = errors.New("a database access group has no leader")
	ErrNoApprovalGroups   = errors.New("migration database has no group requiring approval")
	ErrLeaderMustBeMember = errors.New("group leader must be a member of the group")
	ErrApprovalDuplicate  = errors.New("group leader has already recorded a decision")
)

type MigrationDefinition struct {
	ID             int64          `json:"id"`
	DatabaseID     int64          `json:"database_id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	CreatedBy      int64          `json:"created_by"`
	WorkflowStatus WorkflowStatus `json:"workflow_status"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Revision       int64          `json:"revision"`
}

type MigrationRevision struct {
	ID                 int64          `json:"id"`
	DefinitionID       int64          `json:"definition_id"`
	Revision           int64          `json:"revision"`
	UpSQL              string         `json:"up_sql"`
	DownSQL            string         `json:"down_sql,omitempty"`
	Irreversible       bool           `json:"irreversible"`
	IrreversibleReason string         `json:"irreversible_reason,omitempty"`
	WorkflowStatus     WorkflowStatus `json:"workflow_status"`
	CreatedBy          int64          `json:"created_by"`
	CreatedAt          time.Time      `json:"created_at"`
	SubmittedAt        *time.Time     `json:"submitted_at,omitempty"`
	ExecutedAt         *time.Time     `json:"executed_at,omitempty"`
	ExecutionID        string         `json:"execution_id,omitempty"`
}

type Approval struct {
	ID         int64     `json:"id"`
	RevisionID int64     `json:"revision_id"`
	GroupID    int64     `json:"group_id"`
	UserID     int64     `json:"user_id"`
	Decision   string    `json:"decision"`
	DecidedAt  time.Time `json:"decided_at"`
}

type ReviewStatus struct {
	DefinitionID       int64          `json:"definition_id"`
	RevisionID         int64          `json:"revision_id"`
	Revision           int64          `json:"revision"`
	Status             WorkflowStatus `json:"status"`
	RequiredApprovals  int            `json:"required_approvals"`
	CompletedApprovals int            `json:"completed_approvals"`
	RejectedApprovals  int            `json:"rejected_approvals"`
	MissingLeaders     int            `json:"missing_leaders"`
	Ready              bool           `json:"ready"`
}

type GroupLeader struct {
	GroupID      int64  `json:"group_id"`
	LeaderUserID *int64 `json:"leader_user_id"`
}

type RevisionInput struct {
	Name               string
	Description        string
	UpSQL              string
	DownSQL            string
	Irreversible       bool
	IrreversibleReason string
}

// WorkflowRepository persists the Milestone 2 domain independently from the
// synchronous M1 migration API and the worker queue.
type WorkflowRepository struct{ DB *sql.DB }

// WorkflowService applies authorization and workflow rules over the repository.
type WorkflowService struct {
	Repo        WorkflowRepository
	Authz       Authorizer
	Revalidator *Revalidator
}

func NewWorkflowService(db *sql.DB, authz Authorizer) *WorkflowService {
	return &WorkflowService{Repo: WorkflowRepository{DB: db}, Authz: authz, Revalidator: NewRevalidator(db, authz)}
}

func validateRevisionInput(in RevisionInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 || strings.TrimSpace(in.UpSQL) == "" || len(in.UpSQL) > maxSQLBytes || strings.ContainsRune(in.UpSQL, 0) {
		return ErrInvalidRevision
	}
	if in.Irreversible {
		if strings.TrimSpace(in.IrreversibleReason) == "" {
			return ErrIrreversibleReason
		}
	} else if strings.TrimSpace(in.DownSQL) == "" || len(in.DownSQL) > maxSQLBytes || strings.ContainsRune(in.DownSQL, 0) {
		return ErrInvalidRevision
	}
	return nil
}

func (s *WorkflowService) canEdit(ctx context.Context, userID, databaseID int64) error {
	if userID <= 0 || databaseID <= 0 || s.Authz == nil {
		return ErrInvalidArgument
	}
	ok, err := s.Authz.CanEdit(ctx, userID, databaseID)
	if err != nil {
		return fmt.Errorf("check edit access: %w", err)
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

func (s *WorkflowService) canView(ctx context.Context, userID, databaseID int64) error {
	if userID <= 0 || databaseID <= 0 || s.Authz == nil {
		return ErrInvalidArgument
	}
	ok, err := s.Authz.CanView(ctx, userID, databaseID)
	if err != nil {
		return fmt.Errorf("check view access: %w", err)
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// CreateMigration creates a definition and its first immutable-content draft revision.
func (s *WorkflowService) CreateMigration(ctx context.Context, userID, databaseID int64, in RevisionInput) (MigrationDefinition, MigrationRevision, error) {
	if err := s.canEdit(ctx, userID, databaseID); err != nil {
		return MigrationDefinition{}, MigrationRevision{}, err
	}
	if err := validateRevisionInput(in); err != nil {
		return MigrationDefinition{}, MigrationRevision{}, err
	}
	return s.Repo.Create(ctx, userID, databaseID, in)
}

// UpdateMigration appends a new draft revision. Existing submitted or executed
// revisions are never rewritten; this also preserves the exact review snapshot.
func (s *WorkflowService) UpdateMigration(ctx context.Context, userID, definitionID int64, in RevisionInput) (MigrationRevision, error) {
	if definitionID <= 0 || userID <= 0 {
		return MigrationRevision{}, ErrInvalidArgument
	}
	if err := validateRevisionInput(in); err != nil {
		return MigrationRevision{}, err
	}
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return MigrationRevision{}, err
	}
	if err := s.canEdit(ctx, userID, d.DatabaseID); err != nil {
		return MigrationRevision{}, err
	}
	return s.Repo.AppendRevision(ctx, userID, definitionID, in)
}

func (s *WorkflowService) ListMigrations(ctx context.Context, userID, databaseID int64) ([]MigrationDefinition, error) {
	if err := s.canView(ctx, userID, databaseID); err != nil {
		return nil, err
	}
	return s.Repo.ListDefinitions(ctx, databaseID)
}

func (s *WorkflowService) GetMigration(ctx context.Context, userID, definitionID int64) (MigrationDefinition, error) {
	if userID <= 0 || definitionID <= 0 {
		return MigrationDefinition{}, ErrInvalidArgument
	}
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return d, err
	}
	if err := s.canView(ctx, userID, d.DatabaseID); err != nil {
		return d, err
	}
	return d, nil
}

func (s *WorkflowService) ListRevisions(ctx context.Context, userID, definitionID int64) ([]MigrationRevision, error) {
	d, err := s.GetMigration(ctx, userID, definitionID)
	if err != nil {
		return nil, err
	}
	return s.Repo.ListRevisions(ctx, d.ID)
}

func (s *WorkflowService) GetRevision(ctx context.Context, userID, definitionID, revision int64) (MigrationRevision, error) {
	d, err := s.GetMigration(ctx, userID, definitionID)
	if err != nil {
		return MigrationRevision{}, err
	}
	return s.Repo.GetRevision(ctx, d.ID, revision)
}

func (s *WorkflowService) ReviewStatus(ctx context.Context, userID, definitionID int64) (ReviewStatus, error) {
	d, err := s.GetMigration(ctx, userID, definitionID)
	if err != nil {
		return ReviewStatus{}, err
	}
	return s.Repo.ReviewStatus(ctx, d.ID)
}

func (s *WorkflowService) ListApprovals(ctx context.Context, userID, definitionID, revision int64) ([]Approval, error) {
	if _, err := s.GetRevision(ctx, userID, definitionID, revision); err != nil {
		return nil, err
	}
	return s.Repo.ListApprovals(ctx, definitionID, revision)
}

func (s *WorkflowService) GetGroupLeader(ctx context.Context, groupID int64) (GroupLeader, error) {
	if groupID <= 0 {
		return GroupLeader{}, ErrInvalidArgument
	}
	return s.Repo.GetGroupLeader(ctx, groupID)
}

func (s *WorkflowService) SubmitReview(ctx context.Context, userID, definitionID int64) (MigrationRevision, error) {
	if definitionID <= 0 || userID <= 0 {
		return MigrationRevision{}, ErrInvalidArgument
	}
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return MigrationRevision{}, err
	}
	if err := s.canEdit(ctx, userID, d.DatabaseID); err != nil {
		return MigrationRevision{}, err
	}
	return s.Repo.SubmitReview(ctx, definitionID)
}

func (s *WorkflowService) Approve(ctx context.Context, userID, revisionID, groupID int64) (Approval, bool, error) {
	if userID <= 0 || revisionID <= 0 || groupID <= 0 {
		return Approval{}, false, ErrInvalidArgument
	}
	return s.Repo.Approve(ctx, userID, revisionID, groupID)
}

func (s *WorkflowService) ApproveForDefinition(ctx context.Context, userID, definitionID, revisionNumber, groupID int64) (Approval, bool, error) {
	if userID <= 0 || definitionID <= 0 || revisionNumber <= 0 || groupID <= 0 {
		return Approval{}, false, ErrInvalidArgument
	}
	v, err := s.Repo.GetRevision(ctx, definitionID, revisionNumber)
	if err != nil {
		return Approval{}, false, err
	}
	return s.Repo.Approve(ctx, userID, v.ID, groupID)
}

// SubmitJob snapshots the latest approved revision and publishes it through
// the existing durable worker queue. The advisory lock serializes API
// submissions so two requests cannot both pass duplicate-job validation.
func (s *WorkflowService) SubmitJob(ctx context.Context, userID, definitionID int64) (worker.Job, error) {
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return worker.Job{}, err
	}
	if err = s.canEdit(ctx, userID, d.DatabaseID); err != nil {
		return worker.Job{}, err
	}
	revision, err := s.Repo.ApprovedRevision(ctx, definitionID)
	if err != nil {
		return worker.Job{}, err
	}
	executionID, err := newExecutionID()
	if err != nil {
		return worker.Job{}, fmt.Errorf("create execution ID: %w", err)
	}
	job := worker.Job{ExecutionID: executionID, DatabaseID: d.DatabaseID, MigrationID: d.ID, Revision: revision.Revision, ActorID: userID,
		Operation: worker.Apply, Mode: worker.Normal, SQL: revision.UpSQL, ReverseSQL: revision.DownSQL}
	return s.enqueueApprovedOperation(ctx, job)
}

// SubmitRollbackJob queues the down SQL of the latest approved revision. The
// active source execution is derived from append-only worker history.
func (s *WorkflowService) SubmitRollbackJob(ctx context.Context, userID, definitionID int64) (worker.Job, error) {
	return s.submitHistoryOperation(ctx, userID, definitionID, worker.Rollback)
}

// SubmitForwardJob queues the up SQL of the same approved revision after its
// latest successful state transition was a rollback.
func (s *WorkflowService) SubmitForwardJob(ctx context.Context, userID, definitionID int64) (worker.Job, error) {
	return s.submitHistoryOperation(ctx, userID, definitionID, worker.Forward)
}

func (s *WorkflowService) submitHistoryOperation(ctx context.Context, userID, definitionID int64, operation worker.Operation) (worker.Job, error) {
	d, err := s.Repo.GetDefinition(ctx, definitionID)
	if err != nil {
		return worker.Job{}, err
	}
	if err = s.canEdit(ctx, userID, d.DatabaseID); err != nil {
		return worker.Job{}, err
	}
	var sourceExecutionID string
	var revisionNumber int64
	query := `SELECT execution_id,revision FROM migration_worker_history WHERE migration_id=$1 AND operation IN ('apply','forward') ORDER BY committed_at DESC,execution_id DESC LIMIT 1`
	if operation == worker.Forward {
		query = `SELECT execution_id,revision FROM migration_worker_history WHERE migration_id=$1 AND operation='rollback' ORDER BY committed_at DESC,execution_id DESC LIMIT 1`
	}
	err = s.Repo.DB.QueryRowContext(ctx, query, definitionID).Scan(&sourceExecutionID, &revisionNumber)
	if errors.Is(err, sql.ErrNoRows) {
		if operation == worker.Forward {
			return worker.Job{}, ErrForwardUnavailable
		}
		return worker.Job{}, ErrRollbackUnavailable
	}
	if err != nil {
		return worker.Job{}, fmt.Errorf("load migration source execution: %w", err)
	}
	revision, err := s.Repo.GetRevision(ctx, definitionID, revisionNumber)
	if err != nil {
		return worker.Job{}, err
	}
	executionID, err := newExecutionID()
	if err != nil {
		return worker.Job{}, fmt.Errorf("create execution ID: %w", err)
	}
	job := worker.Job{ExecutionID: executionID, DatabaseID: d.DatabaseID, MigrationID: d.ID, Revision: revision.Revision, ActorID: userID,
		Operation: operation, Mode: worker.Normal, SQL: revision.UpSQL, ReverseSQL: revision.DownSQL, SourceExecutionID: sourceExecutionID}
	return s.enqueueApprovedOperation(ctx, job)
}

func (s *WorkflowService) enqueueApprovedOperation(ctx context.Context, job worker.Job) (worker.Job, error) {
	if s.Revalidator == nil {
		return worker.Job{}, worker.ErrUnapproved
	}
	conn, err := s.Repo.DB.Conn(ctx)
	if err != nil {
		return worker.Job{}, fmt.Errorf("lock job submission: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationSubmitLockID); err != nil {
		return worker.Job{}, fmt.Errorf("lock job submission: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationSubmitLockID)
	if err = s.Revalidator.Revalidate(ctx, job); err != nil {
		return worker.Job{}, err
	}
	queued, err := worker.Enqueue(ctx, s.Repo.DB, job)
	if err != nil {
		return worker.Job{}, fmt.Errorf("publish migration job: %w", err)
	}
	return queued, nil
}

func newExecutionID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (s *WorkflowService) RejectForDefinition(ctx context.Context, userID, definitionID, revisionNumber, groupID int64) (Approval, error) {
	if userID <= 0 || definitionID <= 0 || revisionNumber <= 0 || groupID <= 0 {
		return Approval{}, ErrInvalidArgument
	}
	v, err := s.Repo.GetRevision(ctx, definitionID, revisionNumber)
	if err != nil {
		return Approval{}, err
	}
	return s.Repo.Reject(ctx, userID, v.ID, groupID)
}

// ApprovedRevision is the domain execution gate. A submit-job handler must call
// it before enqueueing the returned immutable snapshot.
func (s *WorkflowService) ApprovedRevision(ctx context.Context, definitionID int64) (MigrationRevision, error) {
	return s.Repo.ApprovedRevision(ctx, definitionID)
}

// MarkExecuted records the worker's successful execution against the exact
// approved revision. Callers must invoke this only after the worker reports
// success and pass its durable execution ID.
func (s *WorkflowService) MarkExecuted(ctx context.Context, definitionID, revisionID int64, executionID string) (MigrationRevision, error) {
	if definitionID <= 0 || revisionID <= 0 || strings.TrimSpace(executionID) == "" {
		return MigrationRevision{}, ErrInvalidArgument
	}
	return s.Repo.MarkExecuted(ctx, definitionID, revisionID, executionID)
}

// SetGroupLeader is intended to be called by an authenticated platform-admin
// boundary. It refuses leaders who are not already group members.
func (s *WorkflowService) SetGroupLeader(ctx context.Context, groupID, userID int64) error {
	if groupID <= 0 || userID <= 0 {
		return ErrInvalidArgument
	}
	return s.Repo.SetGroupLeader(ctx, groupID, userID)
}

func (r WorkflowRepository) Create(ctx context.Context, userID, databaseID int64, in RevisionInput) (MigrationDefinition, MigrationRevision, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return MigrationDefinition{}, MigrationRevision{}, err
	}
	defer tx.Rollback()
	var d MigrationDefinition
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_definitions(database_id,name,description,created_by)
		VALUES($1,$2,$3,$4) RETURNING id,database_id,name,description,created_by,workflow_status,created_at,updated_at`, databaseID, strings.TrimSpace(in.Name), in.Description, userID).
		Scan(&d.ID, &d.DatabaseID, &d.Name, &d.Description, &d.CreatedBy, &d.WorkflowStatus, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return d, MigrationRevision{}, fmt.Errorf("create migration definition: %w", err)
	}
	v, err := insertRevision(ctx, tx, userID, d.ID, 1, in)
	if err != nil {
		return d, v, err
	}
	d.Revision = 1
	if err := tx.Commit(); err != nil {
		return d, v, err
	}
	return d, v, nil
}

func (r WorkflowRepository) GetDefinition(ctx context.Context, id int64) (MigrationDefinition, error) {
	var d MigrationDefinition
	err := r.DB.QueryRowContext(ctx, `SELECT d.id,d.database_id,d.name,d.description,d.created_by,
		CASE WHEN EXISTS(SELECT 1 FROM migration_worker_history h WHERE h.migration_id=d.id) THEN 'executed' ELSE d.workflow_status END,
		d.created_at,d.updated_at,
		COALESCE((SELECT MAX(revision) FROM migration_revisions WHERE definition_id=d.id),0)
		FROM migration_definitions d WHERE d.id=$1`, id).
		Scan(&d.ID, &d.DatabaseID, &d.Name, &d.Description, &d.CreatedBy, &d.WorkflowStatus, &d.CreatedAt, &d.UpdatedAt, &d.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrWorkflowNotFound
	}
	return d, err
}

func (r WorkflowRepository) ListDefinitions(ctx context.Context, databaseID int64) ([]MigrationDefinition, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT d.id,d.database_id,d.name,d.description,d.created_by,d.workflow_status,d.created_at,d.updated_at,COALESCE(MAX(r.revision),0)
		FROM migration_definitions d LEFT JOIN migration_revisions r ON r.definition_id=d.id WHERE d.database_id=$1
		GROUP BY d.id ORDER BY d.updated_at DESC,d.id DESC`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MigrationDefinition, 0)
	for rows.Next() {
		var d MigrationDefinition
		if err = rows.Scan(&d.ID, &d.DatabaseID, &d.Name, &d.Description, &d.CreatedBy, &d.WorkflowStatus, &d.CreatedAt, &d.UpdatedAt, &d.Revision); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r WorkflowRepository) ListRevisions(ctx context.Context, definitionID int64) ([]MigrationRevision, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id,definition_id,revision,up_sql,COALESCE(down_sql,''),irreversible,COALESCE(irreversible_reason,''),workflow_status,created_by,created_at,submitted_at,executed_at,COALESCE(execution_id,'') FROM migration_revisions WHERE definition_id=$1 ORDER BY revision DESC`, definitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MigrationRevision, 0)
	for rows.Next() {
		var v MigrationRevision
		if err = rows.Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt, &v.SubmittedAt, &v.ExecutedAt, &v.ExecutionID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r WorkflowRepository) GetRevision(ctx context.Context, definitionID, revision int64) (MigrationRevision, error) {
	var v MigrationRevision
	err := r.DB.QueryRowContext(ctx, `SELECT id,definition_id,revision,up_sql,COALESCE(down_sql,''),irreversible,COALESCE(irreversible_reason,''),workflow_status,created_by,created_at,submitted_at,executed_at,COALESCE(execution_id,'') FROM migration_revisions WHERE definition_id=$1 AND revision=$2`, definitionID, revision).
		Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt, &v.SubmittedAt, &v.ExecutedAt, &v.ExecutionID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrWorkflowNotFound
	}
	return v, err
}

func (r WorkflowRepository) ReviewStatus(ctx context.Context, definitionID int64) (ReviewStatus, error) {
	var s ReviewStatus
	err := r.DB.QueryRowContext(ctx, `SELECT d.id,r.id,r.revision,
		CASE WHEN EXISTS(SELECT 1 FROM migration_worker_history h WHERE h.migration_id=d.id) THEN 'executed' ELSE r.workflow_status END,
		(SELECT COUNT(*) FROM migration_review_groups rg WHERE rg.revision_id=r.id),
		(SELECT COUNT(*) FROM migration_review_groups rg JOIN migration_approvals a ON a.revision_id=rg.revision_id AND a.group_id=rg.group_id AND a.user_id=rg.leader_user_id AND a.decision='approved' WHERE rg.revision_id=r.id),
		(SELECT COUNT(*) FROM migration_review_groups rg JOIN migration_approvals a ON a.revision_id=rg.revision_id AND a.group_id=rg.group_id AND a.user_id=rg.leader_user_id AND a.decision='rejected' WHERE rg.revision_id=r.id),
		CASE WHEN r.workflow_status='draft' THEN (SELECT COUNT(*) FROM group_database_access ga JOIN app_groups g ON g.id=ga.group_id WHERE ga.database_id=d.database_id AND g.leader_user_id IS NULL) ELSE 0 END
		FROM migration_definitions d JOIN LATERAL (SELECT * FROM migration_revisions WHERE definition_id=d.id ORDER BY revision DESC LIMIT 1) r ON TRUE WHERE d.id=$1`, definitionID).
		Scan(&s.DefinitionID, &s.RevisionID, &s.Revision, &s.Status, &s.RequiredApprovals, &s.CompletedApprovals, &s.RejectedApprovals, &s.MissingLeaders)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrWorkflowNotFound
	}
	if err != nil {
		return s, err
	}
	s.Ready = s.Status == WorkflowApproved && s.RequiredApprovals > 0 && s.CompletedApprovals == s.RequiredApprovals && s.MissingLeaders == 0
	return s, nil
}

func (r WorkflowRepository) ListApprovals(ctx context.Context, definitionID, revision int64) ([]Approval, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT a.id,a.revision_id,a.group_id,a.user_id,a.decision,a.decided_at FROM migration_approvals a JOIN migration_revisions r ON r.id=a.revision_id WHERE r.definition_id=$1 AND r.revision=$2 ORDER BY a.group_id,a.decided_at`, definitionID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Approval, 0)
	for rows.Next() {
		var a Approval
		if err = rows.Scan(&a.ID, &a.RevisionID, &a.GroupID, &a.UserID, &a.Decision, &a.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r WorkflowRepository) GetGroupLeader(ctx context.Context, groupID int64) (GroupLeader, error) {
	var g GroupLeader
	var userID sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `SELECT id,leader_user_id FROM app_groups WHERE id=$1`, groupID).Scan(&g.GroupID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrWorkflowNotFound
	}
	if err != nil {
		return g, err
	}
	if userID.Valid {
		g.LeaderUserID = &userID.Int64
	}
	return g, nil
}

func (r WorkflowRepository) AppendRevision(ctx context.Context, userID, definitionID int64, in RevisionInput) (MigrationRevision, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRevision{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationSubmitLockID); err != nil {
		return MigrationRevision{}, fmt.Errorf("lock migration update: %w", err)
	}
	var dID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM migration_definitions WHERE id=$1 FOR UPDATE`, definitionID).Scan(&dID); errors.Is(err, sql.ErrNoRows) {
		return MigrationRevision{}, ErrWorkflowNotFound
	}
	if err != nil {
		return MigrationRevision{}, err
	}
	var activeJob bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs WHERE migration_id=$1 AND status IN ('pending','running','retrying','partial','partially_reverted','uncertain'))`, definitionID).Scan(&activeJob); err != nil {
		return MigrationRevision{}, err
	}
	if activeJob {
		return MigrationRevision{}, ErrExecutionAlreadyQueued
	}
	var rev int64
	var latestStatus WorkflowStatus
	if err = tx.QueryRowContext(ctx, `SELECT revision,workflow_status FROM migration_revisions WHERE definition_id=$1 ORDER BY revision DESC LIMIT 1 FOR UPDATE`, definitionID).Scan(&rev, &latestStatus); err != nil {
		return MigrationRevision{}, err
	}
	if latestStatus == WorkflowInReview {
		return MigrationRevision{}, ErrRevisionImmutable
	}
	rev++
	if _, err = tx.ExecContext(ctx, `UPDATE migration_definitions SET name=$2,description=$3,workflow_status='draft',updated_at=NOW() WHERE id=$1`, definitionID, strings.TrimSpace(in.Name), in.Description); err != nil {
		return MigrationRevision{}, err
	}
	v, err := insertRevision(ctx, tx, userID, definitionID, rev, in)
	if err != nil {
		return v, err
	}
	if err := tx.Commit(); err != nil {
		return v, err
	}
	return v, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, userID, definitionID, revision int64, in RevisionInput) (MigrationRevision, error) {
	var v MigrationRevision
	var down any
	if !in.Irreversible {
		down = strings.TrimSpace(in.DownSQL)
	}
	var reason any
	if in.Irreversible {
		reason = strings.TrimSpace(in.IrreversibleReason)
	}
	err := tx.QueryRowContext(ctx, `INSERT INTO migration_revisions
		(definition_id,revision,up_sql,down_sql,irreversible,irreversible_reason,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		RETURNING id,definition_id,revision,up_sql,COALESCE(down_sql,''),irreversible,COALESCE(irreversible_reason,''),workflow_status,created_by,created_at`,
		definitionID, revision, strings.TrimSpace(in.UpSQL), down, in.Irreversible, reason, userID).
		Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return v, fmt.Errorf("create migration revision: %w", err)
	}
	return v, nil
}

func (r WorkflowRepository) SubmitReview(ctx context.Context, definitionID int64) (MigrationRevision, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRevision{}, err
	}
	defer tx.Rollback()
	var v MigrationRevision
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.definition_id,r.revision,r.up_sql,COALESCE(r.down_sql,''),r.irreversible,
		COALESCE(r.irreversible_reason,''),r.workflow_status,r.created_by,r.created_at
		FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id
		WHERE d.id=$1 ORDER BY r.revision DESC LIMIT 1 FOR UPDATE OF d,r`, definitionID).
		Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrWorkflowNotFound
	}
	if err != nil {
		return v, err
	}
	if v.WorkflowStatus != WorkflowDraft {
		return v, ErrRevisionImmutable
	}
	rows, err := tx.QueryContext(ctx, `SELECT ga.group_id,g.leader_user_id FROM group_database_access ga
		JOIN migration_definitions d ON d.database_id=ga.database_id JOIN app_groups g ON g.id=ga.group_id
		WHERE d.id=$1 ORDER BY ga.group_id FOR SHARE OF ga,g`, definitionID)
	if err != nil {
		return v, err
	}
	type requiredApprover struct {
		groupID  int64
		leaderID sql.NullInt64
	}
	var required []requiredApprover
	for rows.Next() {
		var item requiredApprover
		if err = rows.Scan(&item.groupID, &item.leaderID); err != nil {
			rows.Close()
			return v, err
		}
		required = append(required, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return v, err
	}
	if err = rows.Close(); err != nil {
		return v, err
	}
	if len(required) == 0 {
		return v, ErrNoApprovalGroups
	}
	for _, item := range required {
		if !item.leaderID.Valid {
			return v, ErrGroupLeaderMissing
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO migration_review_groups(revision_id,group_id,leader_user_id) VALUES($1,$2,$3)`, v.ID, item.groupID, item.leaderID.Int64); err != nil {
			return v, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_revisions SET workflow_status='in_review',submitted_at=NOW() WHERE id=$1`, v.ID); err != nil {
		return v, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_definitions SET workflow_status='in_review',updated_at=NOW() WHERE id=$1`, definitionID); err != nil {
		return v, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT submitted_at FROM migration_revisions WHERE id=$1`, v.ID).Scan(&v.SubmittedAt); err != nil {
		return v, err
	}
	v.WorkflowStatus = WorkflowInReview
	if err = tx.Commit(); err != nil {
		return v, err
	}
	return v, nil
}

func (r WorkflowRepository) Approve(ctx context.Context, userID, revisionID, groupID int64) (Approval, bool, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return Approval{}, false, err
	}
	defer tx.Rollback()
	var definitionID int64
	var status WorkflowStatus
	err = tx.QueryRowContext(ctx, `SELECT d.id,r.workflow_status FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id WHERE r.id=$1 FOR UPDATE OF r`, revisionID).Scan(&definitionID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, ErrWorkflowNotFound
	}
	if err != nil {
		return Approval{}, false, err
	}
	if status != WorkflowInReview {
		return Approval{}, false, ErrWorkflowInvalid
	}
	var leader int64
	err = tx.QueryRowContext(ctx, `SELECT leader_user_id FROM migration_review_groups WHERE revision_id=$1 AND group_id=$2 FOR UPDATE`, revisionID, groupID).Scan(&leader)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Approval{}, false, ErrNotGroupLeader
		}
		return Approval{}, false, err
	}
	if leader != userID {
		return Approval{}, false, ErrNotGroupLeader
	}
	var a Approval
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_approvals(revision_id,group_id,user_id,decision) VALUES($1,$2,$3,'approved')
		ON CONFLICT(revision_id,group_id,user_id) DO NOTHING
		RETURNING id,revision_id,group_id,user_id,decision,decided_at`, revisionID, groupID, userID).Scan(&a.ID, &a.RevisionID, &a.GroupID, &a.UserID, &a.Decision, &a.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, ErrApprovalDuplicate
	}
	if err != nil {
		return a, false, fmt.Errorf("record approval: %w", err)
	}
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_review_groups WHERE revision_id=$1)
		AND NOT EXISTS(SELECT 1 FROM migration_review_groups rg LEFT JOIN migration_approvals ma ON ma.revision_id=rg.revision_id AND ma.group_id=rg.group_id AND ma.user_id=rg.leader_user_id AND ma.decision='approved'
		WHERE rg.revision_id=$1 AND ma.id IS NULL)`, revisionID).Scan(&complete)
	if err != nil {
		return a, false, err
	}
	if complete {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_revisions SET workflow_status='approved' WHERE id=$1`, revisionID); err != nil {
			return a, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE migration_definitions SET workflow_status='approved',updated_at=NOW() WHERE id=(SELECT definition_id FROM migration_revisions WHERE id=$1)`, revisionID); err != nil {
			return a, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return a, false, err
	}
	return a, complete, nil
}

func (r WorkflowRepository) Reject(ctx context.Context, userID, revisionID, groupID int64) (Approval, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	var definitionID int64
	var status WorkflowStatus
	err = tx.QueryRowContext(ctx, `SELECT d.id,r.workflow_status FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id WHERE r.id=$1 FOR UPDATE OF r`, revisionID).Scan(&definitionID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrWorkflowNotFound
	}
	if err != nil {
		return Approval{}, err
	}
	if status != WorkflowInReview {
		return Approval{}, ErrWorkflowInvalid
	}
	var leader int64
	err = tx.QueryRowContext(ctx, `SELECT leader_user_id FROM migration_review_groups WHERE revision_id=$1 AND group_id=$2 FOR UPDATE`, revisionID, groupID).Scan(&leader)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotGroupLeader
	}
	if err != nil {
		return Approval{}, err
	}
	if leader != userID {
		return Approval{}, ErrNotGroupLeader
	}
	var approval Approval
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_approvals(revision_id,group_id,user_id,decision) VALUES($1,$2,$3,'rejected') ON CONFLICT(revision_id,group_id,user_id) DO NOTHING
		RETURNING id,revision_id,group_id,user_id,decision,decided_at`, revisionID, groupID, userID).Scan(&approval.ID, &approval.RevisionID, &approval.GroupID, &approval.UserID, &approval.Decision, &approval.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrApprovalDuplicate
	}
	if err != nil {
		return Approval{}, fmt.Errorf("record rejection: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_revisions SET workflow_status='rejected' WHERE id=$1`, revisionID); err != nil {
		return Approval{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_definitions SET workflow_status='rejected',updated_at=NOW() WHERE id=$1`, definitionID); err != nil {
		return Approval{}, err
	}
	if err = tx.Commit(); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

func (r WorkflowRepository) ApprovedRevision(ctx context.Context, definitionID int64) (MigrationRevision, error) {
	var v MigrationRevision
	err := r.DB.QueryRowContext(ctx, `SELECT r.id,r.definition_id,r.revision,r.up_sql,COALESCE(r.down_sql,''),r.irreversible,COALESCE(r.irreversible_reason,''),
		r.workflow_status,r.created_by,r.created_at,r.submitted_at,r.executed_at,COALESCE(r.execution_id,'')
		FROM migration_revisions r WHERE r.definition_id=$1 ORDER BY r.revision DESC LIMIT 1`, definitionID).
		Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt, &v.SubmittedAt, &v.ExecutedAt, &v.ExecutionID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrWorkflowNotFound
	}
	if err != nil {
		return v, err
	}
	if v.WorkflowStatus != WorkflowApproved {
		return v, ErrApprovalIncomplete
	}
	var executed bool
	if err = r.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_worker_history WHERE migration_id=$1)`, definitionID).Scan(&executed); err != nil {
		return v, err
	}
	if executed {
		return v, ErrMigrationAlreadyExecuted
	}
	var complete bool
	err = r.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_review_groups WHERE revision_id=$1)
		AND NOT EXISTS(SELECT 1 FROM migration_review_groups rg LEFT JOIN migration_approvals a ON a.revision_id=rg.revision_id AND a.group_id=rg.group_id AND a.user_id=rg.leader_user_id AND a.decision='approved'
		WHERE rg.revision_id=$1 AND a.id IS NULL)`, v.ID).Scan(&complete)
	if err != nil {
		return v, err
	}
	if !complete {
		return v, ErrApprovalIncomplete
	}
	return v, nil
}

func (r WorkflowRepository) MarkExecuted(ctx context.Context, definitionID, revisionID int64, executionID string) (MigrationRevision, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRevision{}, err
	}
	defer tx.Rollback()
	var v MigrationRevision
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.definition_id,r.revision,r.up_sql,COALESCE(r.down_sql,''),r.irreversible,
		COALESCE(r.irreversible_reason,''),r.workflow_status,r.created_by,r.created_at
		FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id
		WHERE d.id=$1 AND r.id=$2 AND r.id=(SELECT id FROM migration_revisions WHERE definition_id=d.id ORDER BY revision DESC LIMIT 1)
		FOR UPDATE OF d,r`, definitionID, revisionID).Scan(&v.ID, &v.DefinitionID, &v.Revision, &v.UpSQL, &v.DownSQL, &v.Irreversible, &v.IrreversibleReason, &v.WorkflowStatus, &v.CreatedBy, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrWorkflowNotFound
	}
	if err != nil {
		return v, err
	}
	if v.WorkflowStatus != WorkflowApproved {
		return v, ErrApprovalIncomplete
	}
	var complete bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_review_groups WHERE revision_id=$1)
		AND NOT EXISTS(SELECT 1 FROM migration_review_groups rg LEFT JOIN migration_approvals a ON a.revision_id=rg.revision_id AND a.group_id=rg.group_id AND a.user_id=rg.leader_user_id AND a.decision='approved'
		WHERE rg.revision_id=$1 AND a.id IS NULL)`, revisionID).Scan(&complete); err != nil {
		return v, err
	}
	if !complete {
		return v, ErrApprovalIncomplete
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_revisions SET workflow_status='executed',executed_at=NOW(),execution_id=$2 WHERE id=$1`, revisionID, executionID); err != nil {
		return v, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_definitions SET workflow_status='executed',updated_at=NOW() WHERE id=$1`, definitionID); err != nil {
		return v, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT executed_at FROM migration_revisions WHERE id=$1`, revisionID).Scan(&v.ExecutedAt); err != nil {
		return v, err
	}
	v.ExecutionID = executionID
	v.WorkflowStatus = WorkflowExecuted
	if err = tx.Commit(); err != nil {
		return v, err
	}
	return v, nil
}

func (r WorkflowRepository) SetGroupLeader(ctx context.Context, groupID, userID int64) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var member bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_groups WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&member); err != nil {
		return err
	}
	if !member {
		return ErrLeaderMustBeMember
	}
	res, err := tx.ExecContext(ctx, `UPDATE app_groups SET leader_user_id=$2 WHERE id=$1`, groupID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return tx.Commit()
}
