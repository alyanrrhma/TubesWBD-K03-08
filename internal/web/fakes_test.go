package web_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/access"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/admin"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/database"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/web"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

// Fake sederhana agar test handler tidak membutuhkan PostgreSQL. Alur dengan database nyata ada di tests/integration.

type fakeUsers struct {
	mu    sync.Mutex
	next  int64
	users map[string]*fakeUser
}

type fakeUser struct {
	admin.User
	hash string
}

func newFakeUsers(hash string) *fakeUsers {
	f := &fakeUsers{users: map[string]*fakeUser{}}
	for _, u := range []struct {
		name  string
		admin bool
	}{{"admin", true}, {"alice", false}, {"bob", false}} {
		f.next++
		f.users[u.name] = &fakeUser{User: admin.User{ID: f.next, Username: u.name, IsAdmin: u.admin, CreatedAt: time.Now()}, hash: hash}
	}
	return f
}

// auth.UserRepository
func (f *fakeUsers) FindByUsername(_ context.Context, name string) (auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return auth.User{}, sql.ErrNoRows
	}
	return auth.User{ID: u.ID, PasswordHash: u.hash, Admin: u.IsAdmin}, nil
}

func (f *fakeUsers) id(name string) int64 { return f.users[name].ID }

// web.AdminService: hanya method yang dipakai test; sisanya nil -> panic bila terpanggil.
type fakeAdmin struct {
	web.AdminService
	users       *fakeUsers
	groups      []admin.Group
	connections []admin.Connection
	deleteError error
}

func (a *fakeAdmin) GetUser(_ context.Context, id int64) (*admin.User, error) {
	a.users.mu.Lock()
	defer a.users.mu.Unlock()
	for _, u := range a.users.users {
		if u.ID == id {
			cp := u.User
			return &cp, nil
		}
	}
	return nil, admin.ErrNotFound
}

func (a *fakeAdmin) ListUsers(context.Context) ([]admin.User, error) {
	a.users.mu.Lock()
	defer a.users.mu.Unlock()
	var out []admin.User
	for _, u := range a.users.users {
		out = append(out, u.User)
	}
	return out, nil
}

func (a *fakeAdmin) CreateUser(_ context.Context, name, _ string, isAdmin bool) (*admin.User, error) {
	a.users.mu.Lock()
	defer a.users.mu.Unlock()
	if _, ok := a.users.users[name]; ok {
		return nil, &admin.InputError{Message: "Username sudah dipakai."}
	}
	a.users.next++
	u := &fakeUser{User: admin.User{ID: a.users.next, Username: name, IsAdmin: isAdmin, CreatedAt: time.Now()}}
	a.users.users[name] = u
	return &u.User, nil
}

func (a *fakeAdmin) DeleteUser(context.Context, int64) error { return a.deleteError }

func (a *fakeAdmin) ListConnections(context.Context) ([]admin.Connection, error) {
	return a.connections, nil
}

func (a *fakeAdmin) UpdateConnection(_ context.Context, id int64, in admin.ConnectionInput) (*admin.Connection, error) {
	for i := range a.connections {
		if a.connections[i].ID == id {
			c := &a.connections[i]
			c.Name, c.Host, c.Port, c.Database, c.Username, c.SSLMode = in.Name, in.Host, in.Port, in.Database, in.Username, in.SSLMode
			c.Active, c.Complete = true, true
			return c, nil
		}
	}
	return nil, admin.ErrNotFound
}

func (a *fakeAdmin) ListGroups(context.Context) ([]admin.Group, error) { return a.groups, nil }

func (a *fakeAdmin) CreateGroup(_ context.Context, name string) (*admin.Group, error) {
	g := admin.Group{ID: int64(len(a.groups) + 1), Name: name}
	a.groups = append(a.groups, g)
	return &g, nil
}

// fakeAccess menggantikan access.Service dalam unit test viewer dan migration.
type fakeAccess struct{ levels map[[2]int64]access.Level }

func (f fakeAccess) EffectiveAccess(_ context.Context, u, d int64) (access.Level, error) {
	return f.levels[[2]int64{u, d}], nil
}

func (f fakeAccess) Require(ctx context.Context, u, d int64, needed access.Level) error {
	l, _ := f.EffectiveAccess(ctx, u, d)
	if l < needed || l == access.NoAccess {
		return access.ErrForbidden
	}
	return nil
}

// fakeViewer memakai web.DatabaseViewer sungguhan di atas fake registry? Tidak: registry butuh *sql.DB.
// Karena itu viewer di-fake langsung dan memeriksa akses lewat fakeAccess.
type fakeViewer struct {
	access  fakeAccess
	dbs     map[int64]web.DatabaseView
	err     error // bila diisi, semua operasi data mengembalikannya
	rowsFor func(page int) *web.TableDataView
}

func (v *fakeViewer) level(ctx context.Context, u, d int64) (*web.DatabaseView, error) {
	l, _ := v.access.EffectiveAccess(ctx, u, d)
	db, ok := v.dbs[d]
	if l == access.NoAccess || !ok {
		return nil, access.ErrForbidden
	}
	db.Level = l
	return &db, nil
}

func (v *fakeViewer) ListDatabases(ctx context.Context, u int64) ([]web.DatabaseView, error) {
	var out []web.DatabaseView
	for id := range v.dbs {
		if db, err := v.level(ctx, u, id); err == nil {
			out = append(out, *db)
		}
	}
	return out, nil
}

func (v *fakeViewer) Database(ctx context.Context, u, d int64) (*web.DatabaseView, error) {
	return v.level(ctx, u, d)
}

func (v *fakeViewer) data(ctx context.Context, u, d int64) error {
	if _, err := v.level(ctx, u, d); err != nil {
		return err
	}
	return v.err
}

func (v *fakeViewer) Schemas(ctx context.Context, u, d int64) ([]string, error) {
	return []string{"audit", "public"}, v.data(ctx, u, d)
}

func (v *fakeViewer) Tables(ctx context.Context, u, d int64, schema string) ([]database.Table, error) {
	return []database.Table{{Schema: schema, Name: "customers"}}, v.data(ctx, u, d)
}

func (v *fakeViewer) TableDetail(ctx context.Context, u, d int64, schema, table string) (*web.TableDetailView, error) {
	if err := v.data(ctx, u, d); err != nil {
		return nil, err
	}
	return &web.TableDetailView{
		Columns:     []database.Column{{Name: "id", DataType: "integer", Position: 1}, {Name: "email", DataType: "text", IsNullable: true, Position: 2}},
		Constraints: []database.Constraint{{Name: "customers_pkey", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"}},
		Indexes:     []database.Index{{Name: "customers_pkey", Definition: "CREATE UNIQUE INDEX ...", IsUnique: true, IsPrimary: true}},
	}, nil
}

func (v *fakeViewer) TableData(ctx context.Context, u, d int64, schema, table string, page, size int) (*web.TableDataView, error) {
	if err := v.data(ctx, u, d); err != nil {
		return nil, err
	}
	return v.rowsFor(page), nil
}

// recordingMigrations meniru modul P2: View Edit wajib, SQL berisi "fail" -> record FAILED.
type recordingMigrations struct {
	access  fakeAccess
	history []web.MigrationRecord
}

type fakeWorkflow struct {
	web.MigrationWorkflow
	createErr        error
	created          migration.RevisionInput
	getRevision      migration.MigrationRevision
	submitErr        error
	review           migration.ReviewStatus
	approval         migration.Approval
	approvalComplete bool
	approvalErr      error
	approved         migration.MigrationRevision
	approvedErr      error
	rejectErr        error
	job              worker.Job
	submitJobErr     error
}

func (f *fakeWorkflow) CreateMigration(_ context.Context, userID, databaseID int64, in migration.RevisionInput) (migration.MigrationDefinition, migration.MigrationRevision, error) {
	f.created = in
	if f.createErr != nil {
		return migration.MigrationDefinition{}, migration.MigrationRevision{}, f.createErr
	}
	if userID <= 0 || databaseID <= 0 || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.UpSQL) == "" || (!in.Irreversible && strings.TrimSpace(in.DownSQL) == "") {
		return migration.MigrationDefinition{}, migration.MigrationRevision{}, migration.ErrInvalidRevision
	}
	return migration.MigrationDefinition{ID: 7, DatabaseID: databaseID, Name: in.Name, WorkflowStatus: migration.WorkflowDraft, Revision: 1}, migration.MigrationRevision{ID: 9, DefinitionID: 7, Revision: 1, UpSQL: in.UpSQL, DownSQL: in.DownSQL, WorkflowStatus: migration.WorkflowDraft}, nil
}

func (f *fakeWorkflow) GetRevision(_ context.Context, _ int64, definitionID, revision int64) (migration.MigrationRevision, error) {
	v := f.getRevision
	if v.ID == 0 {
		v = migration.MigrationRevision{ID: 9, DefinitionID: definitionID, Revision: revision, WorkflowStatus: migration.WorkflowInReview}
	}
	return v, nil
}
func (f *fakeWorkflow) SubmitReview(_ context.Context, _ int64, definitionID int64) (migration.MigrationRevision, error) {
	if f.submitErr != nil {
		return migration.MigrationRevision{}, f.submitErr
	}
	return migration.MigrationRevision{ID: 9, DefinitionID: definitionID, Revision: 1, WorkflowStatus: migration.WorkflowInReview}, nil
}
func (f *fakeWorkflow) ReviewStatus(context.Context, int64, int64) (migration.ReviewStatus, error) {
	if f.review.Status == "" {
		return migration.ReviewStatus{DefinitionID: 7, RevisionID: 9, Revision: 1, Status: migration.WorkflowInReview, RequiredApprovals: 2, CompletedApprovals: 1}, nil
	}
	return f.review, nil
}
func (f *fakeWorkflow) Approve(context.Context, int64, int64, int64) (migration.Approval, bool, error) {
	return f.approval, f.approvalComplete, f.approvalErr
}
func (f *fakeWorkflow) ApproveForDefinition(ctx context.Context, userID, definitionID, revision, groupID int64) (migration.Approval, bool, error) {
	return f.Approve(ctx, userID, revision, groupID)
}
func (f *fakeWorkflow) RejectForDefinition(context.Context, int64, int64, int64, int64) (migration.Approval, error) {
	return migration.Approval{Decision: "rejected"}, f.rejectErr
}
func (f *fakeWorkflow) ApprovedRevision(context.Context, int64) (migration.MigrationRevision, error) {
	if f.approvedErr != nil {
		return migration.MigrationRevision{}, f.approvedErr
	}
	if f.approved.ID == 0 {
		return migration.MigrationRevision{ID: 9, DefinitionID: 7, Revision: 1, WorkflowStatus: migration.WorkflowApproved}, nil
	}
	return f.approved, nil
}
func (f *fakeWorkflow) SubmitJob(context.Context, int64, int64) (worker.Job, error) {
	if f.submitJobErr != nil {
		return worker.Job{}, f.submitJobErr
	}
	if f.job.ID == "" {
		f.job = worker.Job{ID: "job-123", ExecutionID: "exec-456", Status: worker.Pending}
	}
	return f.job, nil
}
func (f *fakeWorkflow) SubmitRollbackJob(ctx context.Context, userID, definitionID int64) (worker.Job, error) {
	return f.SubmitJob(ctx, userID, definitionID)
}
func (f *fakeWorkflow) SubmitForwardJob(ctx context.Context, userID, definitionID int64) (worker.Job, error) {
	return f.SubmitJob(ctx, userID, definitionID)
}

func (m *recordingMigrations) History(ctx context.Context, u, d int64, page, pageSize int) ([]web.MigrationRecord, int, error) {
	if err := m.access.Require(ctx, u, d, access.ViewOnly); err != nil {
		return nil, 0, err
	}
	total := len(m.history)
	start := (page - 1) * pageSize
	if start >= total {
		return nil, total, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return m.history[start:end], total, nil
}

func (m *recordingMigrations) Detail(
	ctx context.Context,
	userID, databaseID, version int64,
) (*web.MigrationRecord, error) {
	if err := m.access.Require(ctx, userID, databaseID, access.ViewOnly); err != nil {
		return nil, err
	}

	for _, record := range m.history {
		if int64(record.Version) == version {
			result := record
			return &result, nil
		}
	}

	return nil, migration.ErrMigrationNotFound
}

func (m *recordingMigrations) Execute(ctx context.Context, u, d int64, sql string) (*web.MigrationRecord, error) {
	if err := m.access.Require(ctx, u, d, access.ViewEdit); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sql) == "" {
		return nil, migration.ErrEmptySQL
	}
	rec := web.MigrationRecord{ID: int64(len(m.history) + 1), SQL: sql, ExecutedBy: "tester", ExecutedAt: time.Now()}
	if strings.Contains(strings.ToLower(sql), "fail") {
		rec.Status, rec.ErrorMessage = web.MigrationFailed, "syntax error at or near \"fail\""
	} else {
		rec.Status, rec.Version = web.MigrationSuccess, len(m.history)+1
	}
	m.history = append([]web.MigrationRecord{rec}, m.history...)
	return &rec, nil
}

func (m *recordingMigrations) Explain(ctx context.Context, u, d int64, sql string, analyze bool) (string, error) {
	if err := m.access.Require(ctx, u, d, access.ViewEdit); err != nil {
		return "", err
	}
	if strings.TrimSpace(sql) == "" {
		return "", migration.ErrEmptySQL
	}
	return `[{"Plan":{"Node Type":"Result","Startup Cost":0.00,"Total Cost":0.01,"Plan Rows":1}}]`, nil
}

func (m *recordingMigrations) DiffSchema(ctx context.Context, u, d int64, desiredJSON string) (migration.SchemaDiffResult, error) {
	if err := m.access.Require(ctx, u, d, access.ViewEdit); err != nil {
		return migration.SchemaDiffResult{}, err
	}
	return migration.SchemaDiffResult{
		Changes: []migration.SchemaChange{{Kind: "DROP TABLE", Object: "old_table", Description: "Menghapus tabel beserta seluruh datanya.", Destructive: true}},
		SQL:     `DROP TABLE "public"."old_table";`,
	}, nil
}

func (m *recordingMigrations) CurrentSchema(ctx context.Context, u, d int64, schema string) (string, error) {
	if err := m.access.Require(ctx, u, d, access.ViewEdit); err != nil {
		return "", err
	}
	return `{"schema":"public","tables":[{"name":"customers","columns":[{"name":"id","type":"bigint","nullable":false},{"name":"email","type":"text","nullable":false}]}]}`, nil
}
