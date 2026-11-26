package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/migration"
)

func apiCSRF(t *testing.T, e *env, c *http.Client) string {
	t.Helper()
	resp, body := e.get(c, "/api/csrf")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("csrf status=%d body=%s", resp.StatusCode, body)
	}
	var envelope struct {
		Data struct {
			Token string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Token == "" {
		t.Fatalf("missing csrf token: %s", body)
	}
	return envelope.Data.Token
}

func apiRequest(t *testing.T, e *env, c *http.Client, method, path, body, csrf string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	return e.do(c, req)
}

func TestAPIWorkflowUnauthenticatedReturnsJSON401(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(e.client(), "/api/databases/100/migrations")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" || strings.Contains(body, "<html") {
		t.Fatalf("status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func TestMigrationHistoryUsesSeparateRouteFromWorkflowCollection(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")

	resp, body := e.get(c, "/api/databases/100/migration-history?page=1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"items"`) {
		t.Fatalf("history status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/databases/100/migrations", `{"sql":"DROP TABLE customers"}`, apiCSRF(t, e, c))
	if resp.StatusCode != http.StatusBadRequest || len(e.migs.history) != 0 {
		t.Fatalf("legacy execution status=%d body=%s history=%d", resp.StatusCode, body, len(e.migs.history))
	}
}

func TestAPIWorkflowCreateAndValidateInput(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	valid := `{"name":"add_accounts","description":"create account table","up_sql":"CREATE TABLE accounts(id BIGINT)","down_sql":"DROP TABLE accounts"}`
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/databases/100/migrations", valid, csrf)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"workflow_status":"DRAFT"`) || !strings.Contains(body, `"revision":1`) {
		t.Fatalf("unexpected create response: %s", body)
	}
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/databases/100/migrations", `{"name":"bad","up_sql":"CREATE TABLE x(id int)"}`, csrf)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid_request"`) {
		t.Fatalf("invalid input status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowCSRFAndDomainForbidden(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	payload := `{"name":"add_accounts","up_sql":"CREATE TABLE accounts(id BIGINT)","down_sql":"DROP TABLE accounts"}`
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/databases/100/migrations", payload, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"code":"csrf_rejected"`) {
		t.Fatalf("csrf status=%d body=%s", resp.StatusCode, body)
	}
	_ = apiCSRF(t, e, c)
	e.workflow.createErr = migration.ErrForbidden
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/databases/100/migrations", payload, apiCSRF(t, e, c))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"code":"forbidden"`) {
		t.Fatalf("forbidden status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowReviewStatusAndReadiness(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/submit-review", `{}`, csrf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"workflow_status":"PENDING_REVIEW"`) {
		t.Fatalf("submit review status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = e.get(c, "/api/migrations/7/review")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"PENDING_REVIEW"`) || !strings.Contains(body, `"completed_approvals":1`) {
		t.Fatalf("review status=%d body=%s", resp.StatusCode, body)
	}
	e.workflow.review = migration.ReviewStatus{DefinitionID: 7, RevisionID: 9, Revision: 1, Status: migration.WorkflowApproved, RequiredApprovals: 2, CompletedApprovals: 2, Ready: true}
	resp, body = e.get(c, "/api/migrations/7/readiness")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ready":true`) || !strings.Contains(body, `"workflow_status":"APPROVED"`) {
		t.Fatalf("readiness status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowMapsApprovalErrors(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	payload := `{"group_id":3}`
	e.workflow.approvalErr = migration.ErrNotGroupLeader
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/revisions/1/approvals", payload, csrf)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"code":"not_group_leader"`) {
		t.Fatalf("non-leader status=%d body=%s", resp.StatusCode, body)
	}
	e.workflow.approvalErr = migration.ErrWorkflowInvalid
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/revisions/1/approvals", payload, csrf)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"workflow_conflict"`) {
		t.Fatalf("invalid revision status=%d body=%s", resp.StatusCode, body)
	}
	e.workflow.approvalErr = migration.ErrApprovalDuplicate
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/revisions/1/approvals", payload, csrf)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"duplicate_approval"`) {
		t.Fatalf("duplicate approval status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowRejectRecordsDecision(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/revisions/1/rejection", `{"group_id":3}`, csrf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"decision":"rejected"`) || !strings.Contains(body, `"workflow_status":"REJECTED"`) {
		t.Fatalf("rejection status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowSubmitJobReturnsIdentifiersWithoutWaiting(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/submit", `{}`, csrf)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"job_id":"job-123"`) || !strings.Contains(body, `"execution_id":"exec-456"`) || !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("submit status=%d body=%s", resp.StatusCode, body)
	}
	e.workflow.submitJobErr = migration.ErrDatabaseExecutionBlocked
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/submit", `{}`, csrf)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"database_execution_blocked"`) {
		t.Fatalf("enqueue error status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowSubmitJobReturnsImmediatelyWithIDs(t *testing.T) {
	e := newEnv(t)
	c := e.login("alice")
	csrf := apiCSRF(t, e, c)
	resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/submit", `{}`, csrf)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"job_id":"job-123"`) || !strings.Contains(body, `"execution_id":"exec-456"`) || !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("submit status=%d body=%s", resp.StatusCode, body)
	}
	e.workflow.submitJobErr = migration.ErrExecutionAlreadyQueued
	resp, body = apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/submit", `{}`, csrf)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"execution_already_queued"`) {
		t.Fatalf("enqueue failure status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAPIWorkflowRollbackAndForwardReturnQueuedIdentifiers(t *testing.T) {
	for _, route := range []string{"rollback", "forward"} {
		t.Run(route, func(t *testing.T) {
			e := newEnv(t)
			c := e.login("alice")
			csrf := apiCSRF(t, e, c)
			resp, body := apiRequest(t, e, c, http.MethodPost, "/api/migrations/7/"+route, `{}`, csrf)
			if resp.StatusCode != http.StatusAccepted || !strings.Contains(body, `"job_id":"job-123"`) || !strings.Contains(body, `"execution_id":"exec-456"`) {
				t.Fatalf("%s status=%d body=%s", route, resp.StatusCode, body)
			}
		})
	}
}
