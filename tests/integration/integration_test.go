package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/access"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/admin"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/config"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/database"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/platformdb"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/web"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
	"github.com/joho/godotenv"
)

type stack struct {
	t         *testing.T
	ctx       context.Context
	db        *sql.DB
	target    *sql.DB
	targetDSN string
	admin     *admin.Service
	ts        *httptest.Server
	suffix    string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	dsn, targetDSN := os.Getenv("TEST_PLATFORM_DATABASE_URL"), os.Getenv("TEST_TARGET_DATABASE_URL")
	if dsn == "" || targetDSN == "" {
		t.Skip("TEST_PLATFORM_DATABASE_URL dan TEST_TARGET_DATABASE_URL harus di-set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	db, err := platformdb.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	target, err := database.OpenTarget(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })

	cfg, err := config.FromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewManager(auth.PostgresStore{DB: db}, cfg.Session)
	if err != nil {
		t.Fatal(err)
	}
	accessSvc := access.Service{Repo: access.PostgresRepository{DB: db}}
	registry := database.NewRegistry()
	t.Cleanup(func() { registry.CloseAll() })
	adminSvc, err := admin.New(db, registry, sessions, cfg.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	migrationSvc := migration.NewService(db, registry, migration.AccessAuthorizer{Access: accessSvc})
	workflowSvc := migration.NewWorkflowService(db, migration.AccessAuthorizer{Access: accessSvc})
	srv, err := web.New(web.Deps{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: sessions, Users: auth.PostgresUsers{DB: db}, Admin: adminSvc,
		Viewer:     web.DatabaseViewer{Access: accessSvc, Conns: adminSvc, Registry: registry},
		Migrations: web.MigrationAdapter{Service: migrationSvc},
		Workflow:   workflowSvc,
		Health:     db.PingContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &stack{t: t, ctx: ctx, db: db, target: target, targetDSN: targetDSN, admin: adminSvc, ts: ts, suffix: fmt.Sprint(time.Now().UnixNano() % 1_000_000_000)}
}

func TestMain(m *testing.M) {
	_ = godotenv.Load("../../.env")
	os.Exit(m.Run())
}

func (s *stack) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *stack) do(c *http.Client, method, path string) (*http.Response, string) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.ts.URL+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.doRequest(c, req)
}

func (s *stack) doRequest(c *http.Client, req *http.Request) (*http.Response, string) {
	s.t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (s *stack) api(c *http.Client, method, path string, payload any, csrf bool) (*http.Response, string) {
	s.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			s.t.Fatal(err)
		}
		body = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, s.ts.URL+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", s.csrf(c))
	}
	return s.doRequest(c, req)
}

func (s *stack) csrf(c *http.Client) string {
	s.t.Helper()
	resp, body := s.api(c, http.MethodGet, "/api/csrf", nil, false)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("csrf API status=%d body=%s", resp.StatusCode, body)
	}
	var result struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil || result.Data.Token == "" {
		s.t.Fatalf("token CSRF API tidak valid: %s (%v)", body, err)
	}
	return result.Data.Token
}

func (s *stack) login(user string) *http.Client {
	s.t.Helper()
	c := s.client()
	resp, body := s.api(c, http.MethodPost, "/api/auth/login", map[string]string{
		"username": user, "password": "password-1234",
	}, false)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("login API %s gagal: %d %s", user, resp.StatusCode, body)
	}
	return c
}

func (s *stack) user(name string, isAdmin bool) {
	s.t.Helper()
	u, err := s.admin.CreateUser(s.ctx, name, "password-1234", isAdmin)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { s.db.Exec(`DELETE FROM users WHERE id=$1`, u.ID) })
}

func groupID(t *testing.T, s *stack, name string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(s.ctx, `SELECT id FROM app_groups WHERE name=$1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func userID(t *testing.T, s *stack, name string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(s.ctx, `SELECT id FROM users WHERE username=$1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEndToEndSPAMigrationReviewAndViewerAccess(t *testing.T) {
	s := newStack(t)
	adminName, viewer, editor, noAccess := "it-admin-"+s.suffix, "it-view-"+s.suffix, "it-edit-"+s.suffix, "it-none-"+s.suffix
	s.user(adminName, true)
	s.user(viewer, false)
	s.user(editor, false)
	s.user(noAccess, false)

	schema := "it_" + s.suffix
	for _, query := range []string{
		`CREATE SCHEMA ` + schema,
		`CREATE TABLE ` + schema + `.items (id integer primary key, nama text not null, catatan text)`,
		`INSERT INTO ` + schema + `.items SELECT g, 'item ' || g, CASE WHEN g % 2 = 0 THEN NULL ELSE '<b>x</b>' END FROM generate_series(1, 75) g`,
	} {
		if _, err := s.target.ExecContext(s.ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { s.target.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`) })

	adm := s.login(adminName)
	tu, err := url.Parse(s.targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(tu.Port())
	if port == 0 {
		port = 5432
	}
	password, _ := tu.User.Password()
	connection := map[string]any{
		"name": "it-conn-" + s.suffix, "host": tu.Hostname(), "port": port,
		"database": strings.TrimPrefix(tu.Path, "/"), "username": tu.User.Username(),
		"password": password, "ssl_mode": "disable",
	}
	resp, body := s.api(adm, http.MethodPost, "/api/admin/connections", connection, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create connection status=%d body=%s", resp.StatusCode, body)
	}
	connectionName := connection["name"].(string)
	var connectionID int64
	if err := s.db.QueryRowContext(s.ctx, `SELECT id FROM database_connections WHERE name=$1`, connectionName).Scan(&connectionID); err != nil {
		t.Fatalf("koneksi tidak tersimpan: %v", err)
	}
	t.Cleanup(func() { s.db.Exec(`DELETE FROM database_connections WHERE id=$1`, connectionID) })

	primaryGroup, secondGroup := "it-group-"+s.suffix, "it-edit-group-"+s.suffix
	resp, body = s.api(adm, http.MethodPost, "/api/admin/groups", map[string]string{"name": primaryGroup}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group %s: status=%d body=%s", primaryGroup, resp.StatusCode, body)
	}
	primaryID := groupID(t, s, primaryGroup)
	t.Cleanup(func() { s.db.Exec(`DELETE FROM app_groups WHERE id=$1`, primaryID) })

	memberPath := fmt.Sprintf("/api/admin/groups/%d/members", primaryID)
	resp, body = s.api(adm, http.MethodPost, memberPath, map[string]int64{"user_id": userID(t, s, viewer)}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add primary group member: status=%d body=%s", resp.StatusCode, body)
	}
	accessPath := fmt.Sprintf("/api/admin/groups/%d/access", primaryID)
	resp, body = s.api(adm, http.MethodPost, accessPath, map[string]any{"database_id": connectionID, "level": "VIEW_ONLY"}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant View Only access: status=%d body=%s", resp.StatusCode, body)
	}
	leaderPath := fmt.Sprintf("/api/admin/groups/%d/leader", primaryID)
	resp, body = s.api(adm, http.MethodPut, leaderPath, map[string]int64{"user_id": userID(t, s, viewer)}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set primary group leader: status=%d body=%s", resp.StatusCode, body)
	}

	// All browser routes serve the shared SPA shell; data and authorization stay in the API.
	for _, path := range []string{"/login", "/dashboard", "/admin/groups", "/databases/" + strconv.FormatInt(connectionID, 10)} {
		resp, shell := s.do(s.client(), http.MethodGet, path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(shell, `<div id="app"`) {
			t.Fatalf("browser route %s must serve SPA shell: status=%d body=%s", path, resp.StatusCode, shell)
		}
	}

	v := s.login(viewer)
	resp, body = s.do(v, http.MethodGet, "/api/databases")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, connectionName) || !strings.Contains(body, "VIEW_ONLY") {
		t.Fatalf("viewer should see database and access level: status=%d body=%s", resp.StatusCode, body)
	}
	databasePath := "/api/databases/" + strconv.FormatInt(connectionID, 10)
	if resp, body = s.do(v, http.MethodGet, databasePath); resp.StatusCode != http.StatusOK || !strings.Contains(body, schema) {
		t.Fatalf("viewer should read database metadata: status=%d body=%s", resp.StatusCode, body)
	}
	tablesPath := databasePath + "/tables?schema=" + schema
	if resp, body = s.do(v, http.MethodGet, tablesPath); resp.StatusCode != http.StatusOK || !strings.Contains(body, "items") {
		t.Fatalf("viewer should list tables through API: status=%d body=%s", resp.StatusCode, body)
	}
	tablePath := databasePath + "/tables/" + schema + "/items"
	if resp, body = s.do(v, http.MethodGet, tablePath); resp.StatusCode != http.StatusOK || !strings.Contains(body, "nama") {
		t.Fatalf("table detail API status=%d body=%s", resp.StatusCode, body)
	}
	if resp, body = s.do(v, http.MethodGet, tablePath+"/data"); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"Total":75`) || strings.Contains(body, "<b>x</b>") {
		t.Fatalf("table data API should paginate and escape values: status=%d body=%s", resp.StatusCode, body)
	}

	validRevision := map[string]string{
		"name": "approved-table-" + s.suffix, "description": "integration review",
		"up_sql":   "CREATE TABLE " + schema + ".m2_demo (id integer)",
		"down_sql": "DROP TABLE " + schema + ".m2_demo",
	}
	resp, body = s.api(v, http.MethodPost, databasePath+"/migrations", validRevision, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("View Only user must not create migration: status=%d body=%s", resp.StatusCode, body)
	}
	orphan := s.login(noAccess)
	if resp, body = s.do(orphan, http.MethodGet, databasePath); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user without group access must be denied, status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = s.api(adm, http.MethodPost, "/api/admin/groups", map[string]string{"name": secondGroup}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group %s: status=%d body=%s", secondGroup, resp.StatusCode, body)
	}
	secondID := groupID(t, s, secondGroup)
	t.Cleanup(func() { s.db.Exec(`DELETE FROM app_groups WHERE id=$1`, secondID) })
	for _, username := range []string{viewer, editor} {
		path := fmt.Sprintf("/api/admin/groups/%d/members", secondID)
		resp, body = s.api(adm, http.MethodPost, path, map[string]int64{"user_id": userID(t, s, username)}, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("add group member: status=%d body=%s", resp.StatusCode, body)
		}
	}
	secondAccessPath := fmt.Sprintf("/api/admin/groups/%d/access", secondID)
	resp, body = s.api(adm, http.MethodPost, secondAccessPath, map[string]any{"database_id": connectionID, "level": "VIEW_EDIT"}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant View + Edit access: status=%d body=%s", resp.StatusCode, body)
	}
	secondLeaderPath := fmt.Sprintf("/api/admin/groups/%d/leader", secondID)
	resp, body = s.api(adm, http.MethodPut, secondLeaderPath, map[string]int64{"user_id": userID(t, s, viewer)}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set second group leader: status=%d body=%s", resp.StatusCode, body)
	}
	t.Cleanup(func() { s.db.Exec(`DELETE FROM database_connections WHERE id=$1`, connectionID) })

	// The second group grants edit access, while both groups' approvals are snapshotted.
	editorClient := s.login(editor)
	resp, body = s.api(editorClient, http.MethodPost, databasePath+"/migrations", validRevision, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("editor creates M2 draft: status=%d body=%s", resp.StatusCode, body)
	}
	var created struct {
		Data struct {
			Migration struct {
				ID int64 `json:"ID"`
			} `json:"migration"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Data.Migration.ID <= 0 {
		t.Fatalf("create response lacks migration ID: body=%s err=%v", body, err)
	}
	definitionID := created.Data.Migration.ID
	definitionPath := "/api/migrations/" + strconv.FormatInt(definitionID, 10)
	resp, body = s.api(editorClient, http.MethodPost, definitionPath+"/submit-review", map[string]any{}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit review: status=%d body=%s", resp.StatusCode, body)
	}
	reviewer := s.login(viewer)
	for _, id := range []int64{primaryID, secondID} {
		path := definitionPath + "/revisions/1/approvals"
		resp, body = s.api(reviewer, http.MethodPost, path, map[string]int64{"group_id": id}, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("leader approval for group %d: status=%d body=%s", id, resp.StatusCode, body)
		}
	}
	resp, body = s.do(editorClient, http.MethodGet, definitionPath+"/readiness")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ready":true`) {
		t.Fatalf("review should be ready after all group approvals: status=%d body=%s", resp.StatusCode, body)
	}

	// Exercise actual durable apply, rollback, and forward jobs against PostgreSQL.
	applyResp, applyBody := s.api(editorClient, http.MethodPost, definitionPath+"/submit", map[string]any{}, true)
	if applyResp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit apply status=%d body=%s", applyResp.StatusCode, applyBody)
	}
	var applyJob struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(applyBody), &applyJob); err != nil || applyJob.Data.JobID == "" {
		t.Fatalf("apply job response=%s err=%v", applyBody, err)
	}
	runWorkflowWorker(t, s, connectionID)
	assertWorkflowJobStatus(t, s, applyJob.Data.JobID, "success")
	var applied bool
	if err = s.target.QueryRowContext(s.ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+`.m2_demo`).Scan(&applied); err != nil || !applied {
		t.Fatalf("apply did not create target table: applied=%v err=%v", applied, err)
	}

	rollbackJobID := submitOperationReview(t, s, editorClient, reviewer, definitionID, "rollback")
	runWorkflowWorker(t, s, connectionID)
	assertWorkflowJobStatus(t, s, rollbackJobID, "success")
	if err = s.target.QueryRowContext(s.ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+`.m2_demo`).Scan(&applied); err != nil || applied {
		t.Fatalf("rollback did not remove target table: applied=%v err=%v", applied, err)
	}
	forwardJobID := submitOperationReview(t, s, editorClient, reviewer, definitionID, "forward")
	runWorkflowWorker(t, s, connectionID)
	assertWorkflowJobStatus(t, s, forwardJobID, "success")
	if err = s.target.QueryRowContext(s.ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+`.m2_demo`).Scan(&applied); err != nil || !applied {
		t.Fatalf("forward did not recreate target table: applied=%v err=%v", applied, err)
	}
	permissionRaceJob := submitOperationReview(t, s, editorClient, reviewer, definitionID, "rollback")
	resp, body = s.api(adm, http.MethodDelete, fmt.Sprintf("/api/admin/groups/%d/access/%d", primaryID, connectionID), nil, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke permission after queue status=%d body=%s", resp.StatusCode, body)
	}
	runWorkflowWorker(t, s, connectionID)
	assertWorkflowJobStatus(t, s, permissionRaceJob, "failed")
	if err = s.target.QueryRowContext(s.ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+`.m2_demo`).Scan(&applied); err != nil || !applied {
		t.Fatalf("worker executed after approval permission snapshot changed: applied=%v err=%v", applied, err)
	}
	var operationCount int
	if err = s.db.QueryRowContext(s.ctx, `SELECT count(*) FROM migration_worker_history WHERE database_id=$1 AND migration_id=$2`, connectionID, definitionID).Scan(&operationCount); err != nil || operationCount != 3 {
		t.Fatalf("failed permission-revalidated rollback must not add execution history: count=%d err=%v", operationCount, err)
	}

	var migrationHistory int
	if err := s.db.QueryRowContext(s.ctx, `SELECT COUNT(*) FROM migration_history WHERE database_id=$1`, connectionID).Scan(&migrationHistory); err != nil || migrationHistory != 0 {
		t.Fatalf("M2 worker operations must not write legacy synchronous history: count=%d err=%v", migrationHistory, err)
	}
	var rowCount int
	if err := s.target.QueryRowContext(s.ctx, `SELECT COUNT(*) FROM `+schema+`.items`).Scan(&rowCount); err != nil || rowCount != 75 {
		t.Fatalf("migration workflow changed unrelated viewer data: rows=%d err=%v", rowCount, err)
	}
}

func submitOperationReview(t *testing.T, s *stack, editor, reviewer *http.Client, definitionID int64, operation string) string {
	t.Helper()
	path := fmt.Sprintf("/api/migrations/%d/operations/%s/review", definitionID, operation)
	resp, body := s.api(editor, http.MethodPost, path, map[string]any{}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start %s review status=%d body=%s", operation, resp.StatusCode, body)
	}
	var reviewResponse struct {
		Data struct {
			Review struct {
				ID     int64 `json:"id"`
				Groups []struct {
					GroupID int64 `json:"group_id"`
				} `json:"groups"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &reviewResponse); err != nil || reviewResponse.Data.Review.ID <= 0 || len(reviewResponse.Data.Review.Groups) != 2 {
		t.Fatalf("%s review response=%s err=%v", operation, body, err)
	}
	reviewID := reviewResponse.Data.Review.ID
	for _, group := range reviewResponse.Data.Review.Groups {
		resp, body = s.api(reviewer, http.MethodPost, fmt.Sprintf("/api/operation-reviews/%d/approvals", reviewID), map[string]any{"group_id": group.GroupID, "decision": "approved"}, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("approve %s group %d status=%d body=%s", operation, group.GroupID, resp.StatusCode, body)
		}
	}
	resp, body = s.api(editor, http.MethodPost, fmt.Sprintf("/api/operation-reviews/%d/submit", reviewID), map[string]any{}, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit %s review status=%d body=%s", operation, resp.StatusCode, body)
	}
	var submitted struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &submitted); err != nil || submitted.Data.JobID == "" {
		t.Fatalf("submit %s response=%s err=%v", operation, body, err)
	}
	return submitted.Data.JobID
}

func runWorkflowWorker(t *testing.T, s *stack, databaseID int64) {
	t.Helper()
	authorizer := migration.AccessAuthorizer{Access: access.Service{Repo: access.PostgresRepository{DB: s.db}}}
	wf := migration.NewWorkflowService(s.db, authorizer)
	w := &worker.Worker{Platform: s.db, DatabaseID: databaseID, Targets: worker.TargetResolverFunc(func(int64) (*sql.DB, error) { return s.target, nil }), Validator: wf.Revalidator}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("worker RunOnce: %v", err)
	}
}

func assertWorkflowJobStatus(t *testing.T, s *stack, jobID, want string) {
	t.Helper()
	var got string
	if err := s.db.QueryRowContext(s.ctx, `SELECT status FROM migration_jobs WHERE id=$1`, jobID).Scan(&got); err != nil || got != want {
		t.Fatalf("job %s status=%s want %s err=%v", jobID, got, want, err)
	}
}

func TestDeletedUserSessionIsRejectedByAPI(t *testing.T) {
	s := newStack(t)
	name := "it-del-" + s.suffix
	s.user(name, false)
	c := s.login(name)
	if resp, body := s.do(c, http.MethodGet, "/api/auth/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("session API status=%d body=%s", resp.StatusCode, body)
	}
	id := userID(t, s, name)
	if err := s.admin.DeleteUser(s.ctx, id); err != nil {
		t.Fatal(err)
	}
	if resp, body := s.do(c, http.MethodGet, "/api/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("deleted account session must be rejected, status=%d body=%s", resp.StatusCode, body)
	}
	if resp, shell := s.do(c, http.MethodGet, "/dashboard"); resp.StatusCode != http.StatusOK || !strings.Contains(shell, `<div id="app"`) {
		t.Fatalf("browser route remains the SPA shell, status=%d", resp.StatusCode)
	}
}
