package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func dopNoLoginFixture(t *testing.T, store *GormStore) dopBatchItem {
	t.Helper()
	fixture := dopProvisionFixture(t, store, "employee")
	var body dopProvisionRequest
	if err := json.Unmarshal(fixture.Body, &body); err != nil {
		t.Fatal(err)
	}
	body.Authentication, body.OAuthProviderID, body.OAuthSubject = "dop_only", "", ""
	return dopTestItem("employee", "", "", body)
}

func TestDOPProvisionNoLoginCreatesQuotaKeyAndBlocksInteractiveAccess(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	item := dopNoLoginFixture(t, store)
	response := dopBatchRequest(t, handler, "users/provision", "no-login", dopTestItems(item))
	result := dopBatchResults(t, response)[0]
	var value dopProvisionTestResult
	if response.Code != 200 || result.Status != 201 || json.Unmarshal(result.Body, &value) != nil || value.Stage != "ready" || value.Key.Secret == "" || value.User.Status != StatusActive {
		t.Fatalf("DOP-only provision failed: %d %s", response.Code, response.Body)
	}
	var body map[string]any
	if json.Unmarshal(result.Body, &body) != nil || body["authentication"] != "dop_only" {
		t.Fatal("Incorrect authentication mode")
	}
	key := store.ListProjectKeys("dop-project")[0]
	if key.OwnerUserID != value.User.ID || key.ID != value.Key.ID || stringField(value.Quota.Fields, "scope_id") != value.User.ID {
		t.Fatal("Key or quota lost employee ownership")
	}
	login := doJSON(t, handler, http.MethodPost, "/api/admin/auth/login", map[string]any{"identity": value.User.Email, "password": dopOAuthOnlyPassword}, "")
	if login.Code != 401 {
		t.Fatal("DOP-only account obtained a password session")
	}
	guard := &dopIdentityStore{GormStore: store}
	if _, _, err := guard.CreateAdminPasswordResetToken(value.User.ID, "dop-admin", time.Hour); AsHTTPError(err).Status != 403 {
		t.Fatal("DOP-only account enabled password reset")
	}
	api := &dopBatchAPI{server: app, store: store}
	provider, _ := app.findActiveOAuthIdentityProvider("dop-oidc")
	if _, err := api.resolveOAuthUser(provider, map[string]any{"sub": value.User.ID, "email": value.User.Email, "email_verified": true}); err == nil {
		t.Fatal("Unbound SSO claims took over DOP-only user")
	}
	replay := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "no-login", dopTestItems(item)))[0]
	if !replay.Replayed || string(replay.Body) != string(result.Body) || len(store.ListProjectKeys("dop-project")) != 1 {
		t.Fatal("Replay duplicated employee or Key")
	}
	catalog := dopNativeRequest(t, handler, "GET", "/api/admin/users", "", nil, nil)
	if catalog.Code != 200 || strings.Contains(catalog.Body.String(), dopNoLoginProvider) {
		t.Fatal("Internal metadata leaked as OAuth proof")
	}
}

func TestDOPProvisionNoLoginCanBindOAuthLaterWithoutRecreatingResources(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "future-oauth", dopTestItems(dopNoLoginFixture(t, store))))[0]
	var value dopProvisionTestResult
	if json.Unmarshal(result.Body, &value) != nil || result.Status != 201 {
		t.Fatal("Provision failed")
	}
	subject := "real-trusted-sub"
	headers := map[string]string{"X-DOP-OAuth-Provider": "dop-oidc", "X-DOP-OAuth-Subject": base64.RawURLEncoding.EncodeToString([]byte(subject))}
	bound := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/"+value.User.ID, "bind-later", map[string]any{}, headers)
	if bound.Code != 200 {
		t.Fatalf("Future binding failed: %d %s", bound.Code, bound.Body)
	}
	api := &dopBatchAPI{server: app, store: store}
	provider, _ := app.findActiveOAuthIdentityProvider("dop-oidc")
	user, err := api.resolveOAuthUser(provider, map[string]any{"sub": subject, "email_verified": true})
	if err != nil || user.ID != value.User.ID || len(store.ListAdminUsers()) != 2 || len(store.ListProjectKeys("dop-project")) != 1 || len(store.ListResources("quota-policies")) != 1 {
		t.Fatalf("Binding changed employee resources: %v", err)
	}
	headers["X-DOP-OAuth-Subject"] = base64.RawURLEncoding.EncodeToString([]byte("different-sub"))
	if changed := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/"+value.User.ID, "cannot-rebind", map[string]any{}, headers); changed.Code != 409 {
		t.Fatal("Bound identity was mutable")
	}
}

func TestDOPProvisionNoLoginPreservesApprovalAndRejectsIdentityFields(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	item := dopNoLoginFixture(t, store)
	var body dopProvisionRequest
	if err := json.Unmarshal(item.Body, &body); err != nil {
		t.Fatal(err)
	}
	body.OAuthProviderID, body.OAuthSubject = "dop-oidc", "unexpected-sub"
	response := dopBatchRequest(t, handler, "users/provision", "invalid-no-login", dopTestItems(dopTestItem("invalid", "", "", body)))
	if dopBatchResults(t, response)[0].Status != 400 || len(store.ListAdminUsers()) != 1 {
		t.Fatal("DOP-only mode accepted OAuth fields")
	}
	store.CreateResource("approval-flows", AdminResource{ID: "dop-approval", Name: "Approval", Status: StatusActive, Fields: map[string]any{"trigger": "quota_increase"}})
	response = dopBatchRequest(t, handler, "users/provision", "pending-no-login", dopTestItems(item))
	result := dopBatchResults(t, response)[0]
	var value dopProvisionTestResult
	if response.Code != 207 || result.Status != 202 || json.Unmarshal(result.Body, &value) != nil || value.Stage != "quota_pending" || value.User.Status != StatusDisabled || len(store.ListProjectKeys("dop-project")) != 0 {
		t.Fatalf("DOP-only approval result: %d %s", response.Code, response.Body)
	}
}
