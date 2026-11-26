package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

type migrationRequest struct {
	Name               string `json:"name"`
	Description        string `json:"description"`
	UpSQL              string `json:"up_sql"`
	DownSQL            string `json:"down_sql"`
	Irreversible       bool   `json:"irreversible"`
	IrreversibleReason string `json:"irreversible_reason"`
}
type approvalRequest struct {
	GroupID int64 `json:"group_id"`
}
type groupLeaderRequest struct {
	UserID int64 `json:"user_id"`
}
type apiDefinition struct {
	migration.MigrationDefinition
	WorkflowStatus string `json:"workflow_status"`
}
type apiRevision struct {
	migration.MigrationRevision
	WorkflowStatus string `json:"workflow_status"`
}

func apiWorkflowStatus(status migration.WorkflowStatus) string {
	switch status {
	case migration.WorkflowDraft:
		return "DRAFT"
	case migration.WorkflowInReview:
		return "PENDING_REVIEW"
	case migration.WorkflowApproved:
		return "APPROVED"
	case migration.WorkflowRejected:
		return "REJECTED"
	case migration.WorkflowExecuted:
		return "EXECUTED"
	default:
		return string(status)
	}
}
func apiDef(d migration.MigrationDefinition) apiDefinition {
	return apiDefinition{MigrationDefinition: d, WorkflowStatus: apiWorkflowStatus(d.WorkflowStatus)}
}
func apiRev(v migration.MigrationRevision) apiRevision {
	return apiRevision{MigrationRevision: v, WorkflowStatus: apiWorkflowStatus(v.WorkflowStatus)}
}
func apiID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || v <= 0 {
		writeAPIError(w, 400, "invalid_id", "ID harus berupa bilangan positif.")
		return 0, false
	}
	return v, true
}
func apiInput(w http.ResponseWriter, r *http.Request) (migrationRequest, bool) {
	var in migrationRequest
	if err := decodeAPIJSON(w, r, &in); err != nil {
		writeAPIDecodeError(w, err, "Body harus berupa JSON valid.")
		return in, false
	}
	if len(in.Name) > 200 || len(in.UpSQL) > 1<<20 || len(in.DownSQL) > 1<<20 || len(in.IrreversibleReason) > 2000 {
		writeAPIError(w, 400, "invalid_request", "Input melebihi batas yang diizinkan.")
		return in, false
	}
	return in, true
}
func (in migrationRequest) domain() migration.RevisionInput {
	return migration.RevisionInput{Name: in.Name, Description: in.Description, UpSQL: in.UpSQL, DownSQL: in.DownSQL, Irreversible: in.Irreversible, IrreversibleReason: in.IrreversibleReason}
}

func (s *Server) apiCreateMigration(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	dbID, ok := apiID(w, r, "databaseID")
	if !ok {
		return
	}
	in, ok := apiInput(w, r)
	if !ok {
		return
	}
	d, v, err := s.Workflow.CreateMigration(r.Context(), currentUser(r).ID, dbID, in.domain())
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 201, map[string]any{"migration": apiDef(d), "revision": apiRev(v)})
}
func (s *Server) apiListMigrations(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	dbID, ok := apiID(w, r, "databaseID")
	if !ok {
		return
	}
	items, err := s.Workflow.ListMigrations(r.Context(), currentUser(r).ID, dbID)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	out := make([]apiDefinition, 0, len(items))
	for _, d := range items {
		out = append(out, apiDef(d))
	}
	writeAPI(w, 200, map[string]any{"items": out})
}
func (s *Server) apiGetMigration(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	d, err := s.Workflow.GetMigration(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"migration": apiDef(d)})
}
func (s *Server) apiUpdateMigration(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	in, ok := apiInput(w, r)
	if !ok {
		return
	}
	v, err := s.Workflow.UpdateMigration(r.Context(), currentUser(r).ID, id, in.domain())
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"revision": apiRev(v)})
}
func (s *Server) apiListRevisions(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	items, err := s.Workflow.ListRevisions(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	out := make([]apiRevision, 0, len(items))
	for _, v := range items {
		out = append(out, apiRev(v))
	}
	writeAPI(w, 200, map[string]any{"items": out})
}
func (s *Server) apiGetRevision(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	rev, ok := apiID(w, r, "revision")
	if !ok {
		return
	}
	v, err := s.Workflow.GetRevision(r.Context(), currentUser(r).ID, id, rev)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"revision": apiRev(v)})
}
func (s *Server) apiSubmitReview(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	v, err := s.Workflow.SubmitReview(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"revision": apiRev(v)})
}
func (s *Server) apiReviewStatus(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	state, err := s.Workflow.ReviewStatus(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	state.Status = migration.WorkflowStatus(apiWorkflowStatus(state.Status))
	writeAPI(w, 200, state)
}
func (s *Server) apiListApprovals(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	rev, ok := apiID(w, r, "revision")
	if !ok {
		return
	}
	items, err := s.Workflow.ListApprovals(r.Context(), currentUser(r).ID, id, rev)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"items": items})
}
func (s *Server) apiApprove(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	rev, ok := apiID(w, r, "revision")
	if !ok {
		return
	}
	var in approvalRequest
	if err := decodeAPIJSON(w, r, &in); err != nil {
		writeAPIDecodeError(w, err, "Body approval harus berupa JSON valid.")
		return
	}
	if in.GroupID <= 0 {
		writeAPIError(w, 400, "invalid_request", "group_id wajib berupa bilangan positif.")
		return
	}
	approval, complete, err := s.Workflow.ApproveForDefinition(r.Context(), currentUser(r).ID, id, rev, in.GroupID)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"approval": approval, "complete": complete, "definition_id": id})
}

func (s *Server) apiReject(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	definitionID, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	revision, ok := apiID(w, r, "revision")
	if !ok {
		return
	}
	var in approvalRequest
	if err := decodeAPIJSON(w, r, &in); err != nil {
		writeAPIDecodeError(w, err, "Body rejection harus berupa JSON valid.")
		return
	}
	if in.GroupID <= 0 {
		writeAPIError(w, 400, "invalid_request", "group_id wajib berupa bilangan positif.")
		return
	}
	decision, err := s.Workflow.RejectForDefinition(r.Context(), currentUser(r).ID, definitionID, revision, in.GroupID)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"approval": decision, "workflow_status": "REJECTED"})
}
func (s *Server) apiReadiness(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	state, err := s.Workflow.ReviewStatus(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	ready := false
	var v *apiRevision
	if state.Ready {
		approved, readErr := s.Workflow.ApprovedRevision(r.Context(), id)
		if readErr != nil {
			s.apiFail(w, r, readErr)
			return
		}
		converted := apiRev(approved)
		v = &converted
		ready = true
	}
	state.Status = migration.WorkflowStatus(apiWorkflowStatus(state.Status))
	writeAPI(w, 200, map[string]any{"ready": ready, "workflow": state, "revision": v})
}

func (s *Server) apiSubmitJob(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	job, err := s.Workflow.SubmitJob(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "execution_id": job.ExecutionID, "status": string(job.Status)})
}

func (s *Server) apiSubmitRollbackJob(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	s.apiSubmitHistoryJob(w, r, s.Workflow.SubmitRollbackJob)
}

func (s *Server) apiSubmitForwardJob(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	s.apiSubmitHistoryJob(w, r, s.Workflow.SubmitForwardJob)
}

func (s *Server) apiSubmitHistoryJob(w http.ResponseWriter, r *http.Request, submit func(context.Context, int64, int64) (worker.Job, error)) {
	id, ok := apiID(w, r, "definitionID")
	if !ok {
		return
	}
	job, err := submit(r.Context(), currentUser(r).ID, id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "execution_id": job.ExecutionID, "status": string(job.Status)})
}

func (s *Server) apiGetGroupLeader(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "groupID")
	if !ok {
		return
	}
	leader, err := s.Workflow.GetGroupLeader(r.Context(), id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"leader": leader})
}
func (s *Server) apiSetGroupLeader(w http.ResponseWriter, r *http.Request) {
	if !requireWorkflow(s, w) {
		return
	}
	id, ok := apiID(w, r, "groupID")
	if !ok {
		return
	}
	var in groupLeaderRequest
	if err := decodeAPIJSON(w, r, &in); err != nil {
		writeAPIDecodeError(w, err, "Body leader harus berupa JSON valid.")
		return
	}
	if in.UserID <= 0 {
		writeAPIError(w, 400, "invalid_request", "user_id wajib berupa bilangan positif.")
		return
	}
	if err := s.Workflow.SetGroupLeader(r.Context(), id, in.UserID); err != nil {
		s.apiFail(w, r, err)
		return
	}
	leader, err := s.Workflow.GetGroupLeader(r.Context(), id)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeAPI(w, 200, map[string]any{"leader": leader})
}
