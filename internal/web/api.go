package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/access"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/admin"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/database"
)

type apiErrorBody struct {
	Error string `json:"error"`
}

func apiJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func apiError(w http.ResponseWriter, status int, message string) {
	apiJSON(w, status, apiErrorBody{Error: message})
}

func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadCookie := false
		if _, ok := cookieValue(r, auth.CookieName); ok {
			hadCookie = true
		}
		sess, _, err := s.Sessions.Validate(r.Context(), w, r)
		if err != nil {
			if hadCookie {
				s.endSession(w)
			}
			apiError(w, http.StatusUnauthorized, "Sesi tidak valid atau telah berakhir.")
			return
		}
		u, err := s.Admin.GetUser(r.Context(), sess.Principal.UserID)
		if err != nil {
			s.endSession(w)
			apiError(w, http.StatusUnauthorized, "Akun tidak lagi tersedia.")
			return
		}
		rc := &reqCtx{Session: sess, User: userInfo{ID: u.ID, Username: u.Username, IsAdmin: sess.Principal.Admin}}
		rc.CSRF = s.ensureCSRF(w, r, sess)
		next.ServeHTTP(w, r.WithContext(contextWithReq(r, rc)))
	})
}

func contextWithReq(r *http.Request, rc *reqCtx) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, rc)
}

func (s *Server) apiAdmin(next http.Handler) http.Handler {
	return s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin {
			apiError(w, http.StatusForbidden, "Aksi ini hanya tersedia untuk Platform Admin.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) apiCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			apiError(w, http.StatusForbidden, "Permintaan lintas origin ditolak.")
			return
		}
		rc := reqFrom(r.Context())
		if rc == nil || auth.CheckCSRF(rc.Session, r.Header.Get("X-CSRF-Token")) != nil {
			apiError(w, http.StatusForbidden, "Token CSRF tidak valid.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		apiError(w, http.StatusBadRequest, "Format JSON tidak valid.")
		return false
	}
	return true
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		apiError(w, http.StatusForbidden, "Permintaan lintas origin ditolak.")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	u, err := s.Users.FindByUsername(r.Context(), in.Username)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		apiError(w, http.StatusInternalServerError, "Tidak dapat memeriksa kredensial.")
		return
	}
	hash := dummyHash()
	if found {
		hash = u.PasswordHash
	}
	passwordOK := auth.VerifyPassword(hash, in.Password)
	if !found || in.Username == "" || in.Password == "" || !passwordOK {
		apiError(w, http.StatusUnauthorized, "Username atau password salah.")
		return
	}
	auth.LoginService{Users: s.Users, Sessions: s.Sessions}.RevokeCurrent(r.Context(), w, r)
	csrf, err := s.Sessions.Login(r.Context(), w, auth.Principal{UserID: u.ID, Admin: u.Admin})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "Tidak dapat membuat sesi.")
		return
	}
	s.setCSRFCookie(w, csrf)
	apiJSON(w, http.StatusOK, map[string]any{"user": map[string]any{"id": u.ID, "username": u.Username, "is_platform_admin": u.Admin}, "csrf_token": csrf})
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	rc := reqFrom(r.Context())
	apiJSON(w, http.StatusOK, map[string]any{"user": map[string]any{"id": rc.User.ID, "username": rc.User.Username, "is_platform_admin": rc.User.IsAdmin}, "csrf_token": rc.CSRF})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	rc := reqFrom(r.Context())
	if err := s.Sessions.Logout(r.Context(), w, rc.Session); err != nil {
		apiError(w, http.StatusInternalServerError, "Gagal mengakhiri sesi.")
		return
	}
	s.endSession(w)
	apiJSON(w, http.StatusOK, map[string]string{"message": "logged out"})
}

func (s *Server) apiDatabases(w http.ResponseWriter, r *http.Request) {
	items, err := s.Viewer.ListDatabases(r.Context(), currentUser(r).ID)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, d := range items {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name, "host": d.Host, "port": d.Port, "database": d.Database, "level": d.Level.String(), "active": d.Active})
	}
	apiJSON(w, 200, out)
}
func (s *Server) apiDatabase(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	db, err := s.Viewer.Database(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	schemas, err := s.Viewer.Schemas(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, map[string]any{"database": map[string]any{
		"id": db.ID, "name": db.Name, "host": db.Host, "port": db.Port,
		"database": db.Database, "level": db.Level.String(), "active": db.Active,
	}, "schemas": schemas})
}
func (s *Server) apiTables(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	schema := r.URL.Query().Get("schema")
	if schema == "" {
		apiError(w, 400, "Parameter schema wajib diisi.")
		return
	}
	items, err := s.Viewer.Tables(r.Context(), currentUser(r).ID, id, schema)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, items)
}
func (s *Server) apiTableDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	detail, err := s.Viewer.TableDetail(r.Context(), currentUser(r).ID, id, r.PathValue("schema"), r.PathValue("table"))
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, detail)
}
func (s *Server) apiTableData(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	data, err := s.Viewer.TableData(r.Context(), currentUser(r).ID, id, r.PathValue("schema"), r.PathValue("table"), page, dataPageSize)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	pages := (data.Total + dataPageSize - 1) / dataPageSize
	if pages < 1 {
		pages = 1
	}
	apiJSON(w, 200, map[string]any{"data": data, "pages": pages})
}
func (s *Server) apiMigrationHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	items, total, err := s.Migrations.History(r.Context(), currentUser(r).ID, id, page, 20)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, map[string]any{"items": items, "page": page, "page_size": 20, "total": total})
}
func (s *Server) apiMigrationDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 {
		apiError(w, 400, "Versi migration tidak valid.")
		return
	}
	item, err := s.Migrations.Detail(r.Context(), currentUser(r).ID, id, version)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, item)
}
func (s *Server) apiMigrationExecute(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	var in struct {
		SQL string `json:"sql"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	item, err := s.Migrations.Execute(r.Context(), currentUser(r).ID, id, in.SQL)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, http.StatusCreated, item)
}

func (s *Server) apiMigrationExplain(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "databaseID")
	if !ok {
		return
	}
	var in struct {
		SQL     string `json:"sql"`
		Analyze bool   `json:"analyze"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	plan, err := s.Migrations.Explain(r.Context(), currentUser(r).ID, id, in.SQL, in.Analyze)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, http.StatusOK, map[string]any{"plan": plan})
}
func (s *Server) apiServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrForbidden):
		apiError(w, 403, "Akses ditolak.")
	case errors.Is(err, admin.ErrNotFound), errors.Is(err, database.ErrTableNotFound):
		apiError(w, 404, "Data tidak ditemukan.")
	case errors.Is(err, admin.ErrUserHasHistory):
		apiError(w, 409, "Pengguna tidak dapat dihapus karena memiliki riwayat migration.")
	case errors.Is(err, database.ErrInvalidArgument):
		apiError(w, 400, "Parameter permintaan tidak valid.")
	case errors.Is(err, database.ErrConnectFailed), errors.Is(err, database.ErrNotRegistered):
		apiError(w, 503, "Koneksi database target belum tersedia.")
	case errors.Is(err, database.ErrTargetPermissionDenied):
		apiError(w, 502, "Database target menolak akses.")
	case errors.Is(err, database.ErrQueryTimeout):
		apiError(w, 504, "Query melewati batas waktu.")
	default:
		var input *admin.InputError
		if errors.As(err, &input) {
			apiError(w, 422, input.Message)
			return
		}
		s.Logger.Error("API service gagal", "error", err)
		apiError(w, 500, "Terjadi kesalahan saat memproses permintaan.")
	}
}
func (s *Server) apiPathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, 400, "ID tidak valid.")
		return 0, false
	}
	return id, true
}

func (s *Server) apiAdminUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.Admin.ListUsers(r.Context())
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, items)
	case http.MethodPost:
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
			IsAdmin  bool   `json:"is_platform_admin"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		item, err := s.Admin.CreateUser(r.Context(), in.Username, in.Password, in.IsAdmin)
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 201, item)
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}
func (s *Server) apiAdminUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	if r.Method != http.MethodDelete {
		apiError(w, 405, "Method tidak diizinkan.")
		return
	}
	if id == currentUser(r).ID {
		apiError(w, 400, "Tidak dapat menghapus akun yang sedang digunakan.")
		return
	}
	if err := s.Admin.DeleteUser(r.Context(), id); err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, map[string]string{"message": "user deleted"})
}
func (s *Server) apiAdminGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.Admin.ListGroups(r.Context())
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, items)
	case http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		item, err := s.Admin.CreateGroup(r.Context(), in.Name)
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 201, item)
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}
func (s *Server) apiAdminGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	group, err := s.Admin.GetGroup(r.Context(), id)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	members, err := s.Admin.ListMembers(r.Context(), id)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	accessRows, err := s.Admin.ListGroupAccess(r.Context(), id)
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	users, err := s.Admin.ListUsers(r.Context())
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	conns, err := s.Admin.ListConnections(r.Context())
	if err != nil {
		s.apiServiceError(w, err)
		return
	}
	memberSet := map[int64]bool{}
	for _, m := range members {
		memberSet[m.ID] = true
	}
	candidates := []admin.User{}
	for _, u := range users {
		if !memberSet[u.ID] {
			candidates = append(candidates, u)
		}
	}
	apiJSON(w, 200, map[string]any{"group": group, "members": members, "candidates": candidates, "access": accessRows, "connections": conns})
}
func (s *Server) apiAdminGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var in struct {
			UserID int64 `json:"user_id"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.UserID <= 0 {
			apiError(w, 400, "user_id tidak valid.")
			return
		}
		if err := s.Admin.AddMember(r.Context(), id, in.UserID); err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, map[string]string{"message": "member added"})
	case http.MethodDelete:
		uid, ok := s.apiPathID(w, r, "userID")
		if !ok {
			return
		}
		if err := s.Admin.RemoveMember(r.Context(), id, uid); err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, map[string]string{"message": "member removed"})
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}
func (s *Server) apiAdminGroupAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var in struct {
			DatabaseID int64  `json:"database_id"`
			Level      string `json:"level"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		level, err := access.ParseLevel(in.Level)
		if err != nil {
			apiError(w, 400, "Level akses tidak valid.")
			return
		}
		if err = s.Admin.GrantAccess(r.Context(), id, in.DatabaseID, level); err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, map[string]string{"message": "access granted"})
	case http.MethodDelete:
		dbID, ok := s.apiPathID(w, r, "dbID")
		if !ok {
			return
		}
		if err := s.Admin.RevokeAccess(r.Context(), id, dbID); err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, map[string]string{"message": "access revoked"})
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}
func (s *Server) apiAdminConnections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.Admin.ListConnections(r.Context())
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, items)
	case http.MethodPost:
		var in adminConnectionJSON
		if !decodeJSON(w, r, &in) {
			return
		}
		item, err := s.Admin.CreateConnection(r.Context(), in.input())
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 201, item)
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}

type adminConnectionJSON struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
	SSLMode  string `json:"ssl_mode"`
}

func (in adminConnectionJSON) input() admin.ConnectionInput {
	return admin.ConnectionInput{Name: in.Name, Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username, Password: in.Password, SSLMode: in.SSLMode}
}
func (s *Server) apiAdminConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in adminConnectionJSON
		if !decodeJSON(w, r, &in) {
			return
		}
		item, err := s.Admin.UpdateConnection(r.Context(), id, in.input())
		if err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, item)
	case http.MethodDelete:
		if err := s.Admin.DeleteConnection(r.Context(), id); err != nil {
			s.apiServiceError(w, err)
			return
		}
		apiJSON(w, 200, map[string]string{"message": "connection deleted"})
	default:
		apiError(w, 405, "Method tidak diizinkan.")
	}
}
func (s *Server) apiAdminConnectionTest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiPathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.Admin.TestConnection(r.Context(), id); err != nil {
		s.apiServiceError(w, err)
		return
	}
	apiJSON(w, 200, map[string]string{"message": "connection ok"})
}
