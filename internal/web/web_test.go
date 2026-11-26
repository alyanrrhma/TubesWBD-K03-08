package web_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/access"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/admin"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/config"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/database"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/web"
)

var (
	hashOnce sync.Once
	testHash string
)

func passwordHash(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		h, err := auth.HashPassword("password-1234")
		if err != nil {
			panic(err)
		}
		testHash = h
	})
	return testHash
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t              *testing.T
	ts             *httptest.Server
	users          *fakeUsers
	access         fakeAccess
	viewer         *fakeViewer
	migs           *recordingMigrations
	workflow       *fakeWorkflow
	clock          *clock
	mgr            *auth.Manager
	admin          *fakeAdmin
	salesID, invID int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := config.FromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now()}
	mgr, err := auth.NewManager(auth.NewMemoryStore(), cfg.Session)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Now = clk.Now

	users := newFakeUsers(passwordHash(t))
	e := &env{t: t, users: users, clock: clk, mgr: mgr, salesID: 100, invID: 101}
	e.access = fakeAccess{levels: map[[2]int64]access.Level{
		{users.id("alice"), e.salesID}: access.ViewEdit,
		{users.id("bob"), e.salesID}:   access.ViewOnly,
		{users.id("bob"), e.invID}:     access.ViewOnly,
	}}
	e.viewer = &fakeViewer{
		access: e.access,
		dbs: map[int64]web.DatabaseView{
			e.salesID: {ID: e.salesID, Name: "Sales DB", Host: "localhost", Port: 5433, Database: "sales", Active: true},
			e.invID:   {ID: e.invID, Name: "Inventory DB", Host: "localhost", Port: 5433, Database: "inventory", Active: true},
		},
		rowsFor: func(page int) *web.TableDataView {
			d := &web.TableDataView{Columns: []string{"id", "email"}, Page: page, PageSize: 50, Total: 120}
			if page > 3 {
				return d
			}
			start := (page - 1) * 50
			for i := start; i < start+50 && i < 120; i++ {
				cell := web.CellView{Text: "u@example.com"}
				if i%2 == 1 {
					cell = web.CellView{Null: true}
				}
				d.Rows = append(d.Rows, []web.CellView{{Text: "n"}, cell})
			}
			return d
		},
	}
	e.migs = &recordingMigrations{access: e.access}
	e.workflow = &fakeWorkflow{}
	e.admin = &fakeAdmin{
		users: users,
		connections: []admin.Connection{{
			ID: 1, Name: "Target", Host: "localhost", Port: 5433, Database: "dbmp_target",
			Username: "dbmp", SSLMode: "disable", Complete: true,
		}},
	}

	srv, err := web.New(web.Deps{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: mgr, Users: users, Admin: e.admin,
		Viewer: e.viewer, Migrations: e.migs, Workflow: e.workflow,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) do(c *http.Client, req *http.Request) (*http.Response, string) {
	e.t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) get(c *http.Client, path string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	return e.do(c, req)
}

func (e *env) post(c *http.Client, path string, form url.Values) (*http.Response, string) {
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.do(c, req)
}

func (e *env) login(user string) *http.Client {
	e.t.Helper()
	c := e.client()
	rec := httptest.NewRecorder()
	u := e.users.users[user]
	csrf, err := e.mgr.Login(context.Background(), rec, auth.Principal{UserID: u.ID, Admin: u.IsAdmin})
	if err != nil {
		e.t.Fatal(err)
	}
	cookies := append(rec.Result().Cookies(), &http.Cookie{Name: "dbmp_csrf", Value: csrf, Path: "/"})
	base, _ := url.Parse(e.ts.URL)
	c.Jar.SetCookies(base, cookies)
	return c
}

func loginViaAPI(t *testing.T, e *env, c *http.Client, username, password string) (*http.Response, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	return apiRequest(t, e, c, http.MethodPost, "/api/auth/login", string(payload), "")
}

func apiPost(t *testing.T, e *env, c *http.Client, path, body string) (*http.Response, string) {
	t.Helper()
	return apiRequest(t, e, c, http.MethodPost, path, body, apiCSRF(t, e, c))
}

func cookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSPARoutesServeOneShell(t *testing.T) {
	e := newEnv(t)
	var shell string
	for _, path := range []string{
		"/login", "/dashboard", "/admin", "/admin/groups", "/databases/100",
		"/databases/100/migrations", "/databases/100/tables/public/customers",
	} {
		resp, body := e.get(e.client(), path)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<div id="app"`) ||
			!strings.Contains(body, `/static/js/main.js`) {
			t.Errorf("GET %s harus mengirim SPA shell, status=%d", path, resp.StatusCode)
		}
		if shell == "" {
			shell = body
		} else if body != shell {
			t.Errorf("GET %s mengirim dokumen selain shell SPA", path)
		}
	}
}

func TestAPILoginFailureAndSuccess(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	resp, body := loginViaAPI(t, e, c, "alice", "salah-salah")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Username atau password salah.") {
		t.Fatalf("login gagal harus 401 dengan pesan generik, dapat %d: %s", resp.StatusCode, body)
	}
	if cookie(resp, auth.CookieName) != nil {
		t.Fatal("login gagal tidak boleh membuat cookie session")
	}

	resp, body = loginViaAPI(t, e, c, "alice", "password-1234")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"username":"alice"`) {
		t.Fatalf("login API status=%d body=%s", resp.StatusCode, body)
	}
	for _, name := range []string{auth.CookieName, "dbmp_csrf"} {
		ck := cookie(resp, name)
		if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" {
			t.Fatalf("atribut cookie %s tidak sesuai: %+v", name, ck)
		}
	}
	if resp, _ := e.get(c, "/api/auth/me"); resp.StatusCode != http.StatusOK {
		t.Fatalf("sesi login harus dapat dibaca lewat API, status %d", resp.StatusCode)
	}
}

func TestAPILoginRejectsForeignOrigin(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/api/auth/login", strings.NewReader(`{"username":"alice","password":"password-1234"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := e.do(e.client(), req)
	if resp.StatusCode != http.StatusForbidden || cookie(resp, auth.CookieName) != nil {
		t.Fatalf("Origin asing harus ditolak, dapat %d", resp.StatusCode)
	}
}

func TestLegacyLoginFormIsNotAnEndpoint(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.post(e.client(), "/login", url.Values{"username": {"alice"}, "password": {"password-1234"}})
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("login form lama tidak boleh digunakan, status %d", resp.StatusCode)
	}
	if cookie(resp, auth.CookieName) != nil {
		t.Fatal("endpoint login form lama tidak boleh membuat session")
	}
}

func TestSecondAPILoginRevokesPreviousSession(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	if resp, body := loginViaAPI(t, e, c, "alice", "password-1234"); resp.StatusCode != http.StatusOK {
		t.Fatalf("login pertama status=%d body=%s", resp.StatusCode, body)
	}
	u, _ := url.Parse(e.ts.URL)
	var old string
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.CookieName {
			old = ck.Value
		}
	}
	if old == "" {
		t.Fatal("cookie session tidak ada")
	}
	if resp, body := loginViaAPI(t, e, c, "alice", "password-1234"); resp.StatusCode != http.StatusOK {
		t.Fatalf("login kedua status=%d body=%s", resp.StatusCode, body)
	}
	other := e.client()
	other.Jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieName, Value: old, Path: "/"}})
	if resp, _ := e.get(other, "/api/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session lama harus tidak berlaku, dapat %d", resp.StatusCode)
	}
}

func TestAPIListsOnlyAccessibleDatabasesAndChecksDirectAccess(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	_, body := e.get(alice, "/api/databases")
	if !strings.Contains(body, "Sales DB") || strings.Contains(body, "Inventory DB") {
		t.Fatalf("alice hanya boleh melihat Sales DB, body=%s", body)
	}
	bob := e.login("bob")
	_, body = e.get(bob, "/api/databases")
	if !strings.Contains(body, "Sales DB") || !strings.Contains(body, "Inventory DB") {
		t.Fatalf("bob harus melihat kedua database yang diizinkan, body=%s", body)
	}
	if resp, body := e.get(alice, "/api/databases/101"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("akses langsung ke database terlarang harus 403, status=%d body=%s", resp.StatusCode, body)
	}
	if resp, _ := e.get(e.client(), "/api/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API terproteksi harus 401 tanpa session, status %d", resp.StatusCode)
	}
}

func TestLogoutAPIRequiresCSRFAndEndsSession(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	if resp, _ := apiRequest(t, e, c, http.MethodPost, "/api/auth/logout", `{}`, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout tanpa CSRF harus 403, status %d", resp.StatusCode)
	}
	if resp, _ := apiRequest(t, e, c, http.MethodPost, "/api/auth/logout", `{}`, strings.Repeat("a", 43)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout dengan CSRF salah harus 403, status %d", resp.StatusCode)
	}
	token := apiCSRF(t, e, c)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/api/auth/logout", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", token)
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := e.do(c, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin asing harus ditolak, status %d", resp.StatusCode)
	}
	if resp, body := apiRequest(t, e, c, http.MethodPost, "/api/auth/logout", `{}`, token); resp.StatusCode != http.StatusOK {
		t.Fatalf("logout valid status=%d body=%s", resp.StatusCode, body)
	}
	if resp, _ := e.get(c, "/api/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("setelah logout API harus menolak session, status %d", resp.StatusCode)
	}
}

func TestMissingAPICSRFCookieIsReissued(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	u, _ := url.Parse(e.ts.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "dbmp_csrf", Value: "", MaxAge: -1, Path: "/"}})
	resp, body := e.get(c, "/api/csrf")
	if resp.StatusCode != http.StatusOK || cookie(resp, "dbmp_csrf") == nil || !strings.Contains(body, "csrf_token") {
		t.Fatalf("GET API harus menerbitkan ulang cookie CSRF, status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIAuthSessionExpiresAfterIdleTimeout(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	e.clock.Advance(31 * time.Minute)
	if resp, _ := e.get(c, "/api/auth/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session idle harus ditolak oleh API, status %d", resp.StatusCode)
	}
	if resp, _ := e.get(c, "/dashboard"); resp.StatusCode != http.StatusOK {
		t.Fatalf("route browser tetap harus mengirim shell SPA, status %d", resp.StatusCode)
	}
}

func TestAPISessionRotationKeepsUserLoggedIn(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	u, _ := url.Parse(e.ts.URL)
	var old string
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.CookieName {
			old = ck.Value
		}
	}
	e.clock.Advance(16 * time.Minute)
	resp, body := e.get(c, "/api/auth/me")
	rotated := cookie(resp, auth.CookieName)
	if resp.StatusCode != http.StatusOK || rotated == nil || rotated.Value == old {
		t.Fatalf("GET /api/auth/me harus merotasi session, status=%d body=%s cookie=%+v", resp.StatusCode, body, rotated)
	}
}

func TestAdminAPIRestrictsAdminResources(t *testing.T) {
	e := newEnv(t)
	if resp, _ := e.get(e.login("alice"), "/api/admin/users"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin API harus 403, status %d", resp.StatusCode)
	}
	if resp, body := e.get(e.login("admin"), "/api/admin/users"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "alice") {
		t.Fatalf("admin API status=%d body=%s", resp.StatusCode, body)
	}
}

func TestEmptyAdminCollectionsAreJSONArrays(t *testing.T) {
	e := newEnv(t)
	e.admin.groups = nil
	e.admin.connections = nil
	c := e.login("admin")

	for _, path := range []string{"/api/admin/groups", "/api/admin/connections"} {
		resp, body := e.get(c, path)
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Fatalf("empty collection %s must return [], status=%d body=%s", path, resp.StatusCode, body)
		}
	}
}

func TestAdminAPICreatesUsersAndProtectsCurrentAccount(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin")
	resp, body := apiPost(t, e, c, "/api/admin/users", `{"username":"carol","password":"secret-1234"}`)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, "carol") {
		t.Fatalf("create user status=%d body=%s", resp.StatusCode, body)
	}
	resp, _ = apiPost(t, e, c, "/api/admin/users", `{"username":"carol","password":"secret-1234"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate username harus 422, status %d", resp.StatusCode)
	}
	resp, body = apiRequest(t, e, c, http.MethodDelete, "/api/admin/users/1", "", apiCSRF(t, e, c))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "akun yang sedang digunakan") {
		t.Fatalf("akun aktif tidak boleh dihapus, status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAdminAPIReportsUsersWithMigrationHistory(t *testing.T) {
	e := newEnv(t)
	e.admin.deleteError = admin.ErrUserHasHistory
	c := e.login("admin")
	resp, body := apiRequest(t, e, c, http.MethodDelete, "/api/admin/users/3", "", apiCSRF(t, e, c))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "riwayat migration") {
		t.Fatalf("user ber-riwayat harus ditolak, status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAdminAPIJSONEscapesGroupMarkup(t *testing.T) {
	e := newEnv(t)
	resp, body := apiPost(t, e, e.login("admin"), "/api/admin/groups", `{"name":"<script>alert(1)</script>"}`)
	if resp.StatusCode != http.StatusCreated || strings.Contains(body, "<script>") || !strings.Contains(body, `\u003cscript\u003e`) {
		t.Fatalf("API harus mengembalikan JSON ter-escape, status=%d body=%s", resp.StatusCode, body)
	}
}

func TestViewOnlyCannotCreateOrExplainMigrations(t *testing.T) {
	e := newEnv(t)
	bob := e.login("bob")
	e.workflow.createErr = migration.ErrForbidden
	payload := `{"name":"blocked","up_sql":"ALTER TABLE customers ADD COLUMN note TEXT","down_sql":"ALTER TABLE customers DROP COLUMN note"}`
	resp, body := apiPost(t, e, bob, "/api/databases/100/migrations", payload)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"code":"forbidden"`) {
		t.Fatalf("View Only create harus ditolak, status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = apiPost(t, e, bob, "/api/databases/100/migrations/explain", `{"sql":"SELECT * FROM customers"}`)
	if resp.StatusCode != http.StatusForbidden || len(e.migs.history) != 0 {
		t.Fatalf("View Only EXPLAIN harus ditolak, status=%d body=%s", resp.StatusCode, body)
	}
}

func TestMigrationExplainUsesAPIWithoutExecuting(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	resp, body := apiPost(t, e, alice, "/api/databases/100/migrations/explain", `{"sql":"SELECT * FROM customers WHERE id = 1"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Node Type") || !strings.Contains(body, "Result") {
		t.Fatalf("EXPLAIN API harus menampilkan query plan, status=%d body=%s", resp.StatusCode, body)
	}
	if len(e.migs.history) != 0 {
		t.Fatal("EXPLAIN tidak boleh mengeksekusi atau mencatat migration")
	}
}

func TestLegacyMigrationExecutionIsDisabled(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	for _, query := range []string{"ALTER TABLE orders ADD COLUMN note TEXT;", "SELECT fail;", "  \n "} {
		resp, _ := e.post(alice, "/databases/100/migrations", url.Values{"sql": {query}})
		if resp.StatusCode != http.StatusGone && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("legacy migration harus dinonaktifkan, dapat %d untuk %q", resp.StatusCode, query)
		}
	}
	if len(e.migs.history) != 0 {
		t.Fatal("legacy endpoint tidak boleh mengeksekusi SQL")
	}
}

func TestMigrationHistoryPaginationUsesAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	for i := 27; i >= 1; i-- {
		e.migs.history = append(e.migs.history, web.MigrationRecord{
			ID: int64(i), Version: i, SQL: "SELECT 1", Status: web.MigrationSuccess,
			ExecutedBy: "tester", ExecutedAt: time.Now(),
		})
	}
	resp, body := e.get(alice, "/api/databases/100/migration-history?page=2")
	for _, expected := range []string{`"page":2`, `"total":27`, `"Version":7`, `"Version":1`} {
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("pagination API status=%d body=%s missing=%s", resp.StatusCode, body, expected)
		}
	}
}

func TestTableDataPaginationAndNullUseAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	resp, body := e.get(alice, "/api/databases/100/tables/public/customers/data")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"pages":3`) ||
		!strings.Contains(body, `"Page":1`) || !strings.Contains(body, `"Total":120`) || !strings.Contains(body, `"Null":true`) {
		t.Fatalf("data API page 1 tidak sesuai, status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = e.get(alice, "/api/databases/100/tables/public/customers/data?page=3")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"Page":3`) {
		t.Fatalf("data API page 3 tidak sesuai, status=%d body=%s", resp.StatusCode, body)
	}
}

type wrapped struct {
	sentinel error
	detail   string
}

func wrap(s error, d string) error { return wrapped{s, d} }
func (w wrapped) Error() string    { return w.sentinel.Error() + ": " + w.detail }
func (w wrapped) Unwrap() error    { return w.sentinel }

func TestTargetErrorsDoNotLeakDetailsThroughAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice")
	cases := map[error]struct {
		status int
		text   string
	}{
		wrap(database.ErrConnectFailed, "password=s3cr3t"):        {http.StatusServiceUnavailable, "Koneksi database target belum tersedia."},
		wrap(database.ErrNotRegistered, "id 100"):                 {http.StatusServiceUnavailable, "Koneksi database target belum tersedia."},
		wrap(database.ErrTargetPermissionDenied, "host=10.0.0.5"): {http.StatusBadGateway, "Database target menolak akses."},
		wrap(database.ErrTableNotFound, `"x"."y"`):                {http.StatusNotFound, "Data tidak ditemukan."},
		wrap(database.ErrQueryTimeout, "count table rows"):        {http.StatusGatewayTimeout, "Query melewati batas waktu."},
		io.ErrUnexpectedEOF: {http.StatusInternalServerError, "Terjadi kesalahan saat memproses permintaan."},
	}
	for err, want := range cases {
		e.viewer.err = err
		resp, body := e.get(alice, "/api/databases/100")
		if resp.StatusCode != want.status || !strings.Contains(body, want.text) {
			t.Errorf("%v: status=%d want=%d body=%q", err, resp.StatusCode, want.status, body)
		}
		for _, secret := range []string{"s3cr3t", "10.0.0.5", "unexpected EOF"} {
			if strings.Contains(body, secret) {
				t.Errorf("%v: respons membocorkan %q", err, secret)
			}
		}
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(e.client(), "/login")
	for h, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff"} {
		if resp.Header.Get(h) != want {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Error("CSP tidak ada")
	}
	if resp, body := e.get(e.client(), "/healthz"); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}
}

func TestFormatCell(t *testing.T) {
	cases := []struct {
		in   any
		want web.CellView
	}{
		{nil, web.CellView{Null: true}},
		{"", web.CellView{Text: ""}},
		{"NULL", web.CellView{Text: "NULL"}},
		{int64(42), web.CellView{Text: "42"}},
		{[]byte("halo"), web.CellView{Text: "halo"}},
		{[]byte{0xff, 0x00}, web.CellView{Text: `\xff00`}},
		{true, web.CellView{Text: "true"}},
	}
	for _, c := range cases {
		if got := web.FormatCellForTest(c.in); got != c.want {
			t.Errorf("formatCell(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
	long := strings.Repeat("é", 600)
	if got := web.FormatCellForTest(long); len([]rune(got.Text)) != 501 || !strings.HasSuffix(got.Text, "…") {
		t.Fatalf("format panjang harus dipotong dengan ellipsis, dapat %d rune", len([]rune(got.Text)))
	}
}
