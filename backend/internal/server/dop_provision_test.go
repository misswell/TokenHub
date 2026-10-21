package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func dopProvisionFixture(t *testing.T, store *GormStore, ref string) dopBatchItem {
	t.Helper()
	team := store.CreateResource("teams", AdminResource{ID: "dop-team", Name: "Company", Status: StatusActive})
	provider := store.CreateResource("identity-providers", AdminResource{ID: "dop-oidc", Name: "Enterprise OIDC", Status: StatusActive, Fields: map[string]any{"provider_type": "oidc", "client_id": "test-client", "authorize_url": "https://id.example.test/authorize", "token_url": "https://id.example.test/token", "userinfo_url": "https://id.example.test/userinfo"}})
	project := store.CreateProject(Project{ID: "dop-project", Name: "Company", TeamID: team.ID, Status: StatusActive})
	return dopTestItem(ref, "", "", dopProvisionRequest{
		Username: ref, Name: "Employee", Email: ref + "@example.test", TeamID: team.ID, ProjectID: project.ID,
		OAuthProviderID: provider.ID, OAuthSubject: ref, MonthlyLimitUSD: 100,
		Key: json.RawMessage(`{"name":"Employee key","model_access_mode":"inherit","allowed_models":[],"ip_allowlist":[],"limits":{},"rate_limit_rpm":null,"token_limit_tpm":null,"expires_at":null}`),
	})
}

type dopProvisionTestResult struct {
	User  AdminUser     `json:"user"`
	Quota AdminResource `json:"quota"`
	Stage string        `json:"stage"`
	Key   struct {
		ID     string `json:"id"`
		Secret string `json:"api_key"`
	} `json:"key"`
}

func TestDOPProvisionAtomicAndReplay(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	item := dopProvisionFixture(t, store, "employee")
	response := dopBatchRequest(t, handler, "users/provision", "provision", dopTestItems(item))
	result := dopBatchResults(t, response)[0]
	if response.Code != 200 || result.Status != 201 {
		t.Fatalf("provision failed: %d %s", response.Code, response.Body)
	}
	var value dopProvisionTestResult
	if err := json.Unmarshal(result.Body, &value); err != nil || value.User.ID == "" || value.Key.ID == "" || value.Key.Secret == "" || value.Stage != "ready" || value.Quota.ID == "" {
		t.Fatalf("invalid provision response: %s", result.Body)
	}
	var user AdminUser
	if err := store.db.First(&user, "id = ?", value.User.ID).Error; err != nil || user.PasswordHash != dopOAuthOnlyPassword || user.Role != "user" {
		t.Fatalf("employee was not created without a usable password: %v", err)
	}
	if float64Field(value.Quota.Fields, "monthly_cost_usd") != 100 || stringField(value.Quota.Fields, "scope_id") != user.ID {
		t.Fatal("quota is not owned by the employee")
	}
	for _, password := range []string{"", dopOAuthOnlyPassword, "password", "same-password"} {
		if valid, _ := verifyPassword(user.PasswordHash, password); valid {
			t.Fatal("OAuth marker authenticated a local password")
		}
	}
	replay := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "provision", dopTestItems(item)))[0]
	if !replay.Replayed || !bytes.Equal(result.Body, replay.Body) || len(store.ListResources("quota-policies")) != 1 || len(store.ListProjectKeys("dop-project")) != 1 {
		t.Fatal("replay changed or duplicated provisioned resources")
	}
	for _, event := range store.ListAuditEvents() {
		if bytes.Contains([]byte(event.AfterSnapshot), []byte(value.Key.Secret)) || bytes.Contains([]byte(event.AfterSnapshot), []byte(dopOAuthOnlyPassword)) {
			t.Fatal("audit disclosed credentials")
		}
	}
	// A new operation may not silently link or overwrite an existing employee.
	duplicate := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "different", dopTestItems(item)))[0]
	if duplicate.Status != http.StatusConflict {
		t.Fatal("existing identity was linked automatically")
	}
}

func TestDOPProvisionRollbackAndPartialResults(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	good := dopProvisionFixture(t, store, "good")
	var body dopProvisionRequest
	if err := json.Unmarshal(good.Body, &body); err != nil {
		t.Fatal(err)
	}
	body.Username, body.Email, body.OAuthSubject = "bad", "bad@example.test", "bad"
	body.Key = json.RawMessage(`{"name":"Rejected","model_access_mode":"invalid"}`)
	bad := dopTestItem("bad", "", "", body)
	results := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "partial", dopTestItems(bad, good)))
	if results[0].State != "failed" || results[1].State != "succeeded" || len(store.ListAdminUsers()) != 2 || len(store.ListResources("quota-policies")) != 1 || len(store.ListProjectKeys("dop-project")) != 1 {
		t.Fatal("failed Key left partial identity/quota writes or blocked later items")
	}
	var count int64
	if err := store.db.Model(&dopOAuthIdentity{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("failed provision left an OAuth binding")
	}
}
