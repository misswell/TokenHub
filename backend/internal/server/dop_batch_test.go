package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func newDOPBatchTestServer(t *testing.T) (*GormStore, *Server, http.Handler) {
	t.Helper()
	store := NewMemoryStore()
	admin := AdminUser{ID: "dop-admin", Username: "dop-admin", Email: "admin@example.test", Role: "admin", Status: StatusActive, CreatedAt: time.Now().UTC()}
	if err := store.db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	app := New(store)
	handler, err := app.DOPBatchHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, app, handler
}

func dopBatchRequest(t *testing.T, handler http.Handler, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/"+path, bytes.NewReader(encoded))
	r.Header.Set("Authorization", "Bearer dev_admin_token")
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func dopBatchResults(t *testing.T, response *httptest.ResponseRecorder) []dopBatchResult {
	t.Helper()
	var decoded struct {
		Data []dopBatchResult `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.Data == nil {
		t.Fatalf("invalid batch response: %d %s", response.Code, response.Body)
	}
	return decoded.Data
}

func dopTestItem(ref, method, id string, body any) dopBatchItem {
	encoded, _ := json.Marshal(body)
	return dopBatchItem{ClientRef: ref, Method: method, ID: id, Body: encoded}
}

func dopTestItems(items ...dopBatchItem) map[string]any { return map[string]any{"items": items} }

func dopTestUser(t *testing.T, store *GormStore, id string) {
	t.Helper()
	if err := store.db.Create(&AdminUser{ID: id, Username: id, Email: id + "@example.test", Role: "user", Status: StatusActive}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDOPBatchAuthAndOptIn(t *testing.T) {
	_, app, handler := newDOPBatchTestServer(t)
	for _, token := range []string{"", "not-a-token"} {
		r := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/users/usage", strings.NewReader(`{"users":[{"user_id":"dop-admin"}]}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated batch accepted: %d", w.Code)
		}
	}
	if response := doJSON(t, app.Handler(), http.MethodPost, "/api/admin/dop/batch/users/usage", map[string]any{}, ""); response.Code != http.StatusNotFound {
		t.Fatalf("original handler unexpectedly enabled batch API: %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodGet, "/livez", nil, ""); response.Code != http.StatusOK {
		t.Fatalf("native health route failed: %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodGet, "/api/admin/dop/batch/users/usage", nil, ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected method status: %d", response.Code)
	}
}

func TestDOPBatchResourcePartialFailureReplayAndPreservation(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopTestUser(t, store, "employee")
	resource := AdminResource{Name: "Employee monthly", Description: "keep description", Fields: map[string]any{"scope": "user", "scope_id": "employee", "monthly_cost_usd": 50, "daily_requests": 100}}
	good := dopTestItem("employee", http.MethodPost, "", resource)
	bad := dopTestItem("missing", http.MethodPost, "", AdminResource{Name: "Missing user", Fields: map[string]any{"scope": "user", "scope_id": "missing", "monthly_cost_usd": 10}})
	response := dopBatchRequest(t, handler, "user-quotas", "allocate-1", dopTestItems(good, bad))
	results := dopBatchResults(t, response)
	if response.Code != http.StatusMultiStatus || results[0].Status != http.StatusCreated || results[1].Status != http.StatusNotFound {
		t.Fatalf("unexpected partial results: %d %s", response.Code, response.Body)
	}
	var created AdminResource
	if err := json.Unmarshal(results[0].Body, &created); err != nil {
		t.Fatal(err)
	}
	replay := dopBatchResults(t, dopBatchRequest(t, handler, "user-quotas", "allocate-1", dopTestItems(good)))
	if !replay[0].Replayed || !bytes.Equal(replay[0].Body, results[0].Body) || len(store.ListResources("quota-policies")) != 1 {
		t.Fatal("successful item was duplicated on subset retry")
	}
	conflict := good
	conflict.Body = json.RawMessage(`{"name":"different"}`)
	if got := dopBatchResults(t, dopBatchRequest(t, handler, "user-quotas", "allocate-1", dopTestItems(conflict)))[0].Status; got != http.StatusConflict {
		t.Fatalf("expected idempotency conflict, got %d", got)
	}
	patch := dopTestItem("employee", http.MethodPatch, created.ID, AdminResource{Fields: map[string]any{"monthly_cost_usd": 200}})
	response = dopBatchRequest(t, handler, "user-quotas", "allocate-2", dopTestItems(patch))
	if response.Code != http.StatusOK {
		t.Fatalf("patch failed: %s", response.Body)
	}
	updated := store.ListResources("quota-policies")[0]
	if updated.Description != resource.Description || float64Field(updated.Fields, "daily_requests") != 100 || float64Field(updated.Fields, "monthly_cost_usd") != 200 {
		t.Fatalf("patch lost existing fields: %+v", updated)
	}
	for _, event := range store.ListAuditEvents() {
		if event.ActorUserID != "dop-admin" {
			t.Fatal("batch audit lost actor")
		}
	}
}

func TestDOPBatchApprovalsArePendingAndNotDuplicated(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopTestUser(t, store, "employee")
	store.CreateResource("approval-flows", AdminResource{Name: "Quota approval", Fields: map[string]any{"trigger": "quota_increase", "approver_role": "admin"}})
	item := dopTestItem("employee", http.MethodPost, "", AdminResource{Name: "Quota", Fields: map[string]any{"scope": "user", "scope_id": "employee", "monthly_cost_usd": 10}})
	for range 2 {
		response := dopBatchRequest(t, handler, "user-quotas", "approval-1", dopTestItems(item))
		result := dopBatchResults(t, response)[0]
		if response.Code != http.StatusMultiStatus || result.Status != http.StatusAccepted || result.State != "pending" {
			t.Fatalf("approval was reported as applied: %s", response.Body)
		}
	}
	if len(store.ListApprovalRequests()) != 1 || len(store.ListResources("quota-policies")) != 0 {
		t.Fatal("approval was duplicated or bypassed")
	}
}

func TestDOPBatchTeamBudgets(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	team := store.CreateResource("teams", AdminResource{Name: "Company"})
	item := dopTestItem("team", http.MethodPost, "", AdminResource{Name: "Budget", Fields: map[string]any{"scope": "team", "scope_id": team.ID, "period_ref": "", "amount_usd": 1000, "enforcement": "hard"}})
	response := dopBatchRequest(t, handler, "team-budgets", "budget-1", dopTestItems(item))
	if response.Code != http.StatusOK {
		t.Fatalf("budget failed: %s", response.Body)
	}
	var created AdminResource
	if err := json.Unmarshal(dopBatchResults(t, response)[0].Body, &created); err != nil {
		t.Fatal(err)
	}
	patch := dopTestItem("team", http.MethodPatch, created.ID, AdminResource{Fields: map[string]any{"amount_usd": 2000}})
	if response := dopBatchRequest(t, handler, "team-budgets", "budget-2", dopTestItems(patch)); response.Code != http.StatusOK {
		t.Fatalf("budget patch failed: %s", response.Body)
	}
	if count := len(store.ListResources("budgets")); count != 1 {
		t.Fatalf("budget patch created %d budgets", count)
	}
}

func TestDOPBatchKeyReplayEncryptionAndRollback(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopTestUser(t, store, "employee")
	project := store.CreateProject(Project{Name: "Company", Status: StatusActive})
	item := dopTestItem("employee", "", "", map[string]any{"project_id": project.ID, "owner_user_id": "employee", "name": "Employee key", "model_access_mode": "inherit", "limits": QuotaLimits{}})
	response := dopBatchRequest(t, handler, "api-keys/create", "keys-1", dopTestItems(item))
	result := dopBatchResults(t, response)[0]
	if result.Status != http.StatusCreated {
		t.Fatalf("key creation failed: %s", response.Body)
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(result.Body, &created); err != nil || created.APIKey == "" {
		t.Fatal("key secret missing")
	}
	replayed := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/create", "keys-1", dopTestItems(item)))[0]
	if !replayed.Replayed || !bytes.Equal(result.Body, replayed.Body) || len(store.ListProjectKeys(project.ID)) != 1 {
		t.Fatal("key retry did not return the same committed key")
	}
	var receipts []dopBatchReceipt
	if err := store.db.Find(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || !strings.HasPrefix(receipts[0].Ciphertext, "enc:v1:") || strings.Contains(receipts[0].Ciphertext, created.APIKey) {
		t.Fatal("secret receipt not encrypted")
	}
	for _, event := range store.ListAuditEvents() {
		if strings.Contains(event.AfterSnapshot, created.APIKey) || strings.Contains(event.BeforeSnapshot, created.APIKey) {
			t.Fatal("cleartext key leaked into audit")
		}
	}
	if err := store.db.Model(&dopBatchReceipt{}).Where("id = ?", receipts[0].ID).Update("created_at", time.Now().Add(-25*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if got := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/create", "keys-1", dopTestItems(item)))[0].Status; got != http.StatusGone || len(store.ListProjectKeys(project.ID)) != 1 {
		t.Fatal("expired receipt recreated a key")
	}
	// Failure to persist a receipt must roll back the key and its audit.
	if err := store.db.Callback().Update().Before("gorm:update").Register("dop_test_receipt_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "dop_batch_receipts" {
			_ = tx.AddError(fmt.Errorf("injected receipt failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Callback().Update().Remove("dop_test_receipt_failure"); err != nil {
			t.Error(err)
		}
	})
	if got := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/create", "keys-2", dopTestItems(item)))[0].Status; got != http.StatusInternalServerError || len(store.ListProjectKeys(project.ID)) != 1 {
		t.Fatal("receipt failure left an orphan key")
	}
}
