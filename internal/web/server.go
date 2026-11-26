package web

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/config"
	webassets "github.com/alyanrrhma/TubesWBD-K02-08/web"
)

type Deps struct {
	Config     *config.Config
	Logger     *slog.Logger
	Sessions   *auth.Manager
	Users      auth.UserRepository
	Admin      AdminService
	Viewer     Viewer
	Migrations Migrations
	Workflow   MigrationWorkflow
	Health     func(context.Context) error // boleh nil
}

type Server struct {
	Deps
	pages map[string]*pageTemplate
}

func New(d Deps) (*Server, error) {
	pages, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{Deps: d, pages: pages}, nil
}

// Handler mengembalikan seluruh routing aplikasi lengkap dengan middleware global
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webassets.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticCache(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", s.healthz)

	// The SPA uses this API for session and current M1 viewer/admin pages.
	mux.HandleFunc("POST /api/auth/login", s.apiLogin)
	mux.Handle("GET /api/auth/me", s.apiAuth(http.HandlerFunc(s.apiMe)))
	mux.Handle("POST /api/auth/logout", s.apiAuth(s.apiCSRF(http.HandlerFunc(s.apiLogout))))

	mux.Handle("GET /api/databases", s.apiAuth(http.HandlerFunc(s.apiDatabases)))
	mux.Handle("GET /api/databases/{databaseID}", s.apiAuth(http.HandlerFunc(s.apiDatabase)))
	mux.Handle("GET /api/databases/{databaseID}/tables", s.apiAuth(http.HandlerFunc(s.apiTables)))
	mux.Handle("GET /api/databases/{databaseID}/tables/{schema}/{table}", s.apiAuth(http.HandlerFunc(s.apiTableDetail)))
	mux.Handle("GET /api/databases/{databaseID}/tables/{schema}/{table}/data", s.apiAuth(http.HandlerFunc(s.apiTableData)))
	mux.Handle("GET /api/databases/{databaseID}/migration-history", s.apiAuth(http.HandlerFunc(s.apiMigrationHistory)))
	mux.Handle("GET /api/databases/{databaseID}/migrations/{version}", s.apiAuth(http.HandlerFunc(s.apiMigrationDetail)))
	mux.Handle("POST /api/databases/{databaseID}/migrations/explain", s.apiAuth(s.apiCSRF(http.HandlerFunc(s.apiMigrationExplain))))

	// M2 owns the migrations collection: POST creates a draft; execution is queued only after approval.
	workflowAPI := func(h http.HandlerFunc) http.Handler {
		return s.requireAPIAuth(s.csrfProtectAPI(h))
	}
	workflowAdminAPI := func(h http.HandlerFunc) http.Handler {
		return s.requireAPIAuth(s.requireAPIAdmin(s.csrfProtectAPI(h)))
	}
	mux.Handle("GET /api/csrf", s.requireAPIAuth(http.HandlerFunc(s.apiCSRFToken)))
	mux.Handle("GET /api/databases/{databaseID}/migrations", workflowAPI(s.apiListMigrations))
	mux.Handle("POST /api/databases/{databaseID}/migrations", workflowAPI(s.apiCreateMigration))
	mux.Handle("GET /api/migrations/{definitionID}", workflowAPI(s.apiGetMigration))
	mux.Handle("PUT /api/migrations/{definitionID}", workflowAPI(s.apiUpdateMigration))
	mux.Handle("GET /api/migrations/{definitionID}/revisions", workflowAPI(s.apiListRevisions))
	mux.Handle("GET /api/migrations/{definitionID}/revisions/{revision}", workflowAPI(s.apiGetRevision))
	mux.Handle("POST /api/migrations/{definitionID}/submit-review", workflowAPI(s.apiSubmitReview))
	mux.Handle("GET /api/migrations/{definitionID}/review", workflowAPI(s.apiReviewStatus))
	mux.Handle("GET /api/migrations/{definitionID}/revisions/{revision}/approvals", workflowAPI(s.apiListApprovals))
	mux.Handle("POST /api/migrations/{definitionID}/revisions/{revision}/approvals", workflowAPI(s.apiApprove))
	mux.Handle("POST /api/migrations/{definitionID}/revisions/{revision}/rejection", workflowAPI(s.apiReject))
	mux.Handle("GET /api/migrations/{definitionID}/readiness", workflowAPI(s.apiReadiness))
	mux.Handle("POST /api/migrations/{definitionID}/submit", workflowAPI(s.apiSubmitJob))
	mux.Handle("GET /api/jobs/{jobID}", workflowAPI(s.apiGetJobStatus))
	mux.Handle("POST /api/migrations/{definitionID}/rollback", workflowAPI(s.apiSubmitRollbackJob))
	mux.Handle("POST /api/migrations/{definitionID}/forward", workflowAPI(s.apiSubmitForwardJob))
	mux.Handle("GET /api/groups/{groupID}/leader", workflowAPI(s.apiGetGroupLeader))
	mux.Handle("PUT /api/admin/groups/{groupID}/leader", workflowAdminAPI(s.apiSetGroupLeader))

	// Platform Admin API for the SPA.
	mux.Handle("GET /api/admin/users", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminUsers))))
	mux.Handle("POST /api/admin/users", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminUsers))))
	mux.Handle("DELETE /api/admin/users/{id}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminUser))))
	mux.Handle("GET /api/admin/groups", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroups))))
	mux.Handle("POST /api/admin/groups", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroups))))
	mux.Handle("GET /api/admin/groups/{id}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroup))))
	mux.Handle("POST /api/admin/groups/{id}/members", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroupMembers))))
	mux.Handle("DELETE /api/admin/groups/{id}/members/{userID}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroupMembers))))
	mux.Handle("POST /api/admin/groups/{id}/access", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroupAccess))))
	mux.Handle("DELETE /api/admin/groups/{id}/access/{dbID}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminGroupAccess))))
	mux.Handle("GET /api/admin/connections", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminConnections))))
	mux.Handle("POST /api/admin/connections", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminConnections))))
	mux.Handle("PUT /api/admin/connections/{id}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminConnection))))
	mux.Handle("DELETE /api/admin/connections/{id}", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminConnection))))
	mux.Handle("POST /api/admin/connections/{id}/test", s.apiAdmin(s.apiCSRF(http.HandlerFunc(s.apiAdminConnectionTest))))

	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not_found", "Endpoint API tidak ditemukan.")
	})

	// Semua route non-API mengirim satu shell SPA. Navigasi dilakukan di browser.
	mux.HandleFunc("GET /{$}", s.spaShell)
	mux.HandleFunc("GET /", s.spaShell)

	return s.recoverer(s.logging(s.securityHeaders(mux)))
}

func (s *Server) spaShell(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/static/") {
		http.NotFound(w, r)
		return
	}
	data, err := webassets.FS.ReadFile("templates/spa.html")
	if err != nil {
		s.Logger.Error("SPA shell tidak dapat dibaca", "error", err)
		http.Error(w, "Terjadi kesalahan pada server.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.Health != nil {
		if err := s.Health(r.Context()); err != nil {
			s.Logger.Error("healthcheck gagal", "err", err)
			http.Error(w, "platform db unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}
