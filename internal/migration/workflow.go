package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkflowStatus string

const (
	WorkflowDraft    WorkflowStatus = "draft"
	WorkflowInReview WorkflowStatus = "in_review"
	WorkflowApproved WorkflowStatus = "approved"
	WorkflowExecuted WorkflowStatus = "executed"
)

var (
	ErrWorkflowNotFound   = errors.New("migration workflow not found")
	ErrWorkflowInvalid    = errors.New("invalid migration workflow operation")
	ErrIrreversibleReason = errors.New("irreversible migration requires a reason")
	ErrRevisionImmutable  = errors.New("submitted or executed revision is immutable")
	ErrApprovalIncomplete = errors.New("migration revision does not have all required approvals")
	ErrNotGroupLeader     = errors.New("user is not the leader of this database access group")
	ErrGroupLeaderMissing = errors.New("a database access group has no leader")
	ErrNoApprovalGroups   = errors.New("migration database has no group requiring approval")
	ErrLeaderMustBeMember = errors.New("group leader must be a member of the group")
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
	Repo  WorkflowRepository
	Authz Authorizer
}

func NewWorkflowService(db *sql.DB, authz Authorizer) *WorkflowService {
	return &WorkflowService{Repo: WorkflowRepository{DB: db}, Authz: authz}
}

func validateRevisionInput(in RevisionInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 || strings.TrimSpace(in.UpSQL) == "" || len(in.UpSQL) > maxSQLBytes {
		return ErrWorkflowInvalid
	}
	if in.Irreversible {
		if strings.TrimSpace(in.IrreversibleReason) == "" {
			return ErrIrreversibleReason
		}
	} else if strings.TrimSpace(in.DownSQL) == "" || len(in.DownSQL) > maxSQLBytes {
		return ErrWorkflowInvalid
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
	err := r.DB.QueryRowContext(ctx, `SELECT d.id,d.database_id,d.name,d.description,d.created_by,d.workflow_status,d.created_at,d.updated_at,
		COALESCE((SELECT MAX(revision) FROM migration_revisions WHERE definition_id=d.id),0)
		FROM migration_definitions d WHERE d.id=$1`, id).
		Scan(&d.ID, &d.DatabaseID, &d.Name, &d.Description, &d.CreatedBy, &d.WorkflowStatus, &d.CreatedAt, &d.UpdatedAt, &d.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrWorkflowNotFound
	}
	return d, err
}

func (r WorkflowRepository) AppendRevision(ctx context.Context, userID, definitionID int64, in RevisionInput) (MigrationRevision, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return MigrationRevision{}, err
	}
	defer tx.Rollback()
	var dID int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM migration_definitions WHERE id=$1 FOR UPDATE`, definitionID).Scan(&dID); errors.Is(err, sql.ErrNoRows) {
		return MigrationRevision{}, ErrWorkflowNotFound
	}
	if err != nil {
		return MigrationRevision{}, err
	}
	var rev int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM migration_revisions WHERE definition_id=$1`, definitionID).Scan(&rev); err != nil {
		return MigrationRevision{}, err
	}
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
	var missing bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_database_access a JOIN migration_definitions d ON d.database_id=a.database_id
		JOIN app_groups g ON g.id=a.group_id WHERE d.id=$1 AND g.leader_user_id IS NULL)`, definitionID).Scan(&missing); err != nil {
		return v, err
	}
	if missing {
		return v, ErrGroupLeaderMissing
	}
	var hasGroups bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migration_definitions d JOIN group_database_access a ON a.database_id=d.database_id WHERE d.id=$1)`, definitionID).Scan(&hasGroups); err != nil {
		return v, err
	}
	if !hasGroups {
		return v, ErrNoApprovalGroups
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
	var databaseID, definitionID int64
	var status WorkflowStatus
	err = tx.QueryRowContext(ctx, `SELECT d.database_id,d.id,r.workflow_status FROM migration_revisions r JOIN migration_definitions d ON d.id=r.definition_id WHERE r.id=$1 FOR UPDATE OF r`, revisionID).Scan(&databaseID, &definitionID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, false, ErrWorkflowNotFound
	}
	if err != nil {
		return Approval{}, false, err
	}
	if status != WorkflowInReview {
		return Approval{}, false, ErrWorkflowInvalid
	}
	var leader sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT g.leader_user_id FROM app_groups g JOIN group_database_access a ON a.group_id=g.id AND a.database_id=$2 WHERE g.id=$1 FOR UPDATE OF g`, groupID, databaseID).Scan(&leader)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Approval{}, false, ErrNotGroupLeader
		}
		return Approval{}, false, err
	}
	if !leader.Valid || leader.Int64 != userID {
		return Approval{}, false, ErrNotGroupLeader
	}
	var a Approval
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_approvals(revision_id,group_id,user_id,decision) VALUES($1,$2,$3,'approved')
		ON CONFLICT(revision_id,group_id,user_id) DO NOTHING
		RETURNING id,revision_id,group_id,user_id,decision,decided_at`, revisionID, groupID, userID).Scan(&a.ID, &a.RevisionID, &a.GroupID, &a.UserID, &a.Decision, &a.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id,revision_id,group_id,user_id,decision,decided_at FROM migration_approvals WHERE revision_id=$1 AND group_id=$2 AND user_id=$3`, revisionID, groupID, userID).Scan(&a.ID, &a.RevisionID, &a.GroupID, &a.UserID, &a.Decision, &a.DecidedAt)
	}
	if err != nil {
		return a, false, fmt.Errorf("record approval: %w", err)
	}
	var complete bool
	err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(
		SELECT 1 FROM migration_definitions d JOIN group_database_access ga ON ga.database_id=d.database_id
		LEFT JOIN app_groups g ON g.id=ga.group_id LEFT JOIN migration_approvals ma ON ma.revision_id=$2 AND ma.group_id=ga.group_id AND ma.user_id=g.leader_user_id AND ma.decision='approved'
		WHERE d.id=$1 AND (g.leader_user_id IS NULL OR ma.id IS NULL))`, definitionID, revisionID).Scan(&complete)
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
	var complete bool
	err = r.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_database_access ga JOIN migration_definitions d ON d.database_id=ga.database_id WHERE d.id=$1)
		AND NOT EXISTS(SELECT 1 FROM group_database_access ga JOIN migration_definitions d ON d.database_id=ga.database_id
		LEFT JOIN app_groups g ON g.id=ga.group_id LEFT JOIN migration_approvals a ON a.revision_id=$2 AND a.group_id=ga.group_id AND a.user_id=g.leader_user_id AND a.decision='approved'
		WHERE d.id=$1 AND (g.leader_user_id IS NULL OR a.id IS NULL))`, definitionID, v.ID).Scan(&complete)
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
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_database_access ga JOIN migration_definitions d ON d.database_id=ga.database_id WHERE d.id=$1)
		AND NOT EXISTS(SELECT 1 FROM group_database_access ga JOIN migration_definitions d ON d.database_id=ga.database_id
		LEFT JOIN app_groups g ON g.id=ga.group_id LEFT JOIN migration_approvals a ON a.revision_id=$2 AND a.group_id=ga.group_id AND a.user_id=g.leader_user_id AND a.decision='approved'
		WHERE d.id=$1 AND (g.leader_user_id IS NULL OR a.id IS NULL))`, definitionID, revisionID).Scan(&complete); err != nil {
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
