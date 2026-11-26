package web

import "net/http"

// GET /api/jobs/{jobID} is scoped to the requesting user's database access.
func (s *Server) apiGetJobStatus(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	job, err := s.Workflow.GetJobStatus(r.Context(), currentUser(r).ID, r.PathValue("jobID"))
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, http.StatusOK, map[string]any{"job": job})
}
