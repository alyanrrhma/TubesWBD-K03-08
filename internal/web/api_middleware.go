package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/admin"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/auth"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type apiEnvelope struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

func writeAPI(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Data: data})
}
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Error: &apiError{Code: code, Message: message}})
}

func (s *Server) requireAPIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Sessions == nil || s.Admin == nil {
			writeAPIError(w, 500, "server_error", "Permintaan tidak dapat diproses.")
			return
		}
		sess, _, err := s.Sessions.Validate(r.Context(), w, r)
		if errors.Is(err, auth.ErrSessionConflict) {
			sess, _, err = s.Sessions.Validate(r.Context(), w, r)
		}
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				writeAPIError(w, 401, "unauthenticated", "Sesi tidak valid atau telah berakhir.")
			} else {
				s.Logger.Error("autentikasi API gagal", "path", r.URL.Path, "err", err)
				writeAPIError(w, 500, "server_error", "Permintaan tidak dapat diproses.")
			}
			return
		}
		u, err := s.Admin.GetUser(r.Context(), sess.Principal.UserID)
		if err != nil {
			if errors.Is(err, admin.ErrNotFound) {
				writeAPIError(w, 401, "unauthenticated", "Sesi tidak valid atau telah berakhir.")
			} else {
				s.apiFail(w, r, err)
			}
			return
		}
		rc := &reqCtx{Session: sess, User: userInfo{ID: u.ID, Username: u.Username, IsAdmin: sess.Principal.Admin}}
		rc.CSRF = s.ensureCSRF(w, r, sess)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rc)))
	})
}

func (s *Server) requireAPIAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc := reqFrom(r.Context()); rc == nil || !rc.User.IsAdmin {
			writeAPIError(w, 403, "forbidden", "Akses ditolak.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) csrfProtectAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			writeAPIError(w, 403, "csrf_rejected", "Permintaan ditolak.")
			return
		}
		rc := reqFrom(r.Context())
		if rc == nil || auth.CheckCSRF(rc.Session, r.Header.Get("X-CSRF-Token")) != nil {
			writeAPIError(w, 403, "csrf_rejected", "Token CSRF tidak valid atau kedaluwarsa.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) apiCSRF(w http.ResponseWriter, r *http.Request) {
	rc := reqFrom(r.Context())
	if rc == nil || rc.CSRF == "" {
		writeAPIError(w, 500, "server_error", "Token keamanan tidak dapat diterbitkan.")
		return
	}
	writeAPI(w, 200, map[string]string{"csrf_token": rc.CSRF})
}

func decodeAPIJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedAPIContentType
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

var errUnsupportedAPIContentType = errors.New("unsupported API content type")

func writeAPIDecodeError(w http.ResponseWriter, err error, fieldMessage string) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Body JSON melebihi batas 1 MiB.")
	case errors.Is(err, errUnsupportedAPIContentType):
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type harus application/json.")
	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_json", fieldMessage)
	}
}

func (s *Server) apiFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, migration.ErrForbidden):
		writeAPIError(w, 403, "forbidden", "Anda tidak memiliki izin untuk database ini.")
	case errors.Is(err, migration.ErrWorkflowNotFound):
		writeAPIError(w, 404, "not_found", "Migration atau group tidak ditemukan.")
	case errors.Is(err, migration.ErrInvalidArgument), errors.Is(err, migration.ErrInvalidRevision), errors.Is(err, migration.ErrIrreversibleReason):
		writeAPIError(w, 400, "invalid_request", "Data yang dikirim tidak valid.")
	case errors.Is(err, migration.ErrRevisionImmutable):
		writeAPIError(w, 409, "revision_immutable", "Revision tidak dapat diubah pada status saat ini.")
	case errors.Is(err, migration.ErrWorkflowInvalid):
		writeAPIError(w, 409, "workflow_conflict", "Operasi tidak sesuai status workflow saat ini.")
	case errors.Is(err, migration.ErrApprovalIncomplete), errors.Is(err, migration.ErrGroupLeaderMissing), errors.Is(err, migration.ErrNoApprovalGroups):
		writeAPIError(w, 409, "approval_incomplete", "Persyaratan review belum terpenuhi.")
	case errors.Is(err, worker.ErrUnapproved):
		writeAPIError(w, 409, "approval_incomplete", "Revision belum disetujui atau snapshot tidak valid.")
	case errors.Is(err, migration.ErrMigrationAlreadyExecuted):
		writeAPIError(w, 409, "already_executed", "Migration sudah pernah dieksekusi.")
	case errors.Is(err, migration.ErrMigrationOrderInvalid):
		writeAPIError(w, 409, "migration_order_invalid", "Migration belum berada pada urutan eksekusi yang valid.")
	case errors.Is(err, migration.ErrRollbackUnavailable):
		writeAPIError(w, 409, "rollback_unavailable", "Migration bukan migration aktif terakhir.")
	case errors.Is(err, migration.ErrForwardUnavailable):
		writeAPIError(w, 409, "forward_unavailable", "Migration tidak memiliki rollback sukses yang dapat di-forward.")
	case errors.Is(err, migration.ErrIrreversibleRollback):
		writeAPIError(w, 409, "irreversible_migration", "Migration irreversible tidak dapat di-rollback.")
	case errors.Is(err, migration.ErrDatabaseExecutionBlocked):
		writeAPIError(w, 409, "database_execution_blocked", "Database sedang diblokir oleh execution lain.")
	case errors.Is(err, migration.ErrExecutionAlreadyQueued):
		writeAPIError(w, 409, "execution_already_queued", "Migration ini sudah memiliki job aktif atau sukses.")
	case errors.Is(err, migration.ErrJobSnapshotMismatch):
		writeAPIError(w, 409, "revision_snapshot_mismatch", "Job tidak sesuai snapshot revision yang disetujui.")
	case errors.Is(err, migration.ErrNotGroupLeader):
		writeAPIError(w, 403, "not_group_leader", "Hanya leader group terkait yang dapat menyetujui revision.")
	case errors.Is(err, migration.ErrApprovalDuplicate):
		writeAPIError(w, 409, "duplicate_approval", "Group sudah memberikan approval untuk revision ini.")
	case errors.Is(err, migration.ErrLeaderMustBeMember):
		writeAPIError(w, 400, "invalid_group_leader", "Leader harus menjadi anggota group.")
	default:
		s.Logger.Error("workflow API gagal", "path", r.URL.Path, "err", err)
		writeAPIError(w, 500, "server_error", "Permintaan tidak dapat diproses.")
	}
}

func requireWorkflow(s *Server, w http.ResponseWriter) bool {
	if s.Workflow == nil {
		writeAPIError(w, 503, "service_unavailable", "Layanan migration workflow belum tersedia.")
		return false
	}
	return true
}
