package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dopNativeRequest(t *testing.T, handler http.Handler, method, path, key string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	r.Header.Set("Authorization", "Bearer dev_admin_token")
	r.Header.Set("X-DOP-Operation", "true")
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestDOPNativeSingleManagedLifecycle(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopProvisionFixture(t, store, "unused")
	body := map[string]any{"username": "single-employee", "email": "single@example.test", "name": "Single", "role": "user", "status": "disabled", "team_id": "dop-team"}
	headers := map[string]string{"X-DOP-OAuth-Provider": "dop-oidc", "X-DOP-OAuth-Subject": base64.RawURLEncoding.EncodeToString([]byte("exact-subject"))}
	create := dopNativeRequest(t, handler, "POST", "/api/admin/users", "create-single", body, headers)
	if create.Code != 201 {
		t.Fatalf("create: %d %s", create.Code, create.Body)
	}
	var user AdminUser
	if err := json.Unmarshal(create.Body.Bytes(), &user); err != nil || user.ID == "" {
		t.Fatal("missing employee")
	}
	replay := dopNativeRequest(t, handler, "POST", "/api/admin/users", "create-single", body, headers)
	if replay.Code != 201 || replay.Header().Get("X-DOP-Replayed") != "true" || !bytes.Equal(create.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("single replay changed identity")
	}
	path := "/api/admin/users/" + user.ID
	active := map[string]any{"status": "active"}
	premature := dopNativeRequest(t, handler, "PATCH", path, "premature", active, nil)
	if premature.Code != 409 {
		t.Fatalf("opened before quota: %d %s", premature.Code, premature.Body)
	}
	quota := map[string]any{"name": "Personal monthly", "status": "active", "fields": map[string]any{"scope": "user", "scope_id": user.ID, "monthly_cost_usd": 100}}
	q := dopNativeRequest(t, handler, "POST", "/api/admin/resources/quota-policies", "single-quota", quota, nil)
	if q.Code != 201 {
		t.Fatalf("quota: %d %s", q.Code, q.Body)
	}
	enabled := dopNativeRequest(t, handler, "PATCH", path, "activate", active, nil)
	if enabled.Code != 200 {
		t.Fatalf("activation: %d %s", enabled.Code, enabled.Body)
	}
	keyBody := map[string]any{"name": "Employee key", "owner_user_id": user.ID, "model_access_mode": "inherit"}
	key := dopNativeRequest(t, handler, "POST", "/api/admin/projects/dop-project/keys", "single-key", keyBody, nil)
	if key.Code != 201 {
		t.Fatalf("key: %d %s", key.Code, key.Body)
	}
	keyReplay := dopNativeRequest(t, handler, "POST", "/api/admin/projects/dop-project/keys", "single-key", keyBody, nil)
	if keyReplay.Code != 201 || !bytes.Equal(key.Body.Bytes(), keyReplay.Body.Bytes()) || len(store.ListProjectKeys("dop-project")) != 1 {
		t.Fatal("Key replay duplicated or lost secret")
	}
	for _, status := range []string{"disabled", "active"} {
		result := dopNativeRequest(t, handler, "PATCH", path, "state-"+status, map[string]any{"status": status}, nil)
		if result.Code != 200 {
			t.Fatalf("state %s: %d %s", status, result.Code, result.Body)
		}
	}
	usage := dopNativeRequest(t, handler, "GET", "/api/admin/resources/quota-policies?dop_user_id="+user.ID, "", nil, nil)
	if usage.Code != 200 || !strings.Contains(usage.Body.String(), "current_usage") {
		t.Fatalf("usage: %d %s", usage.Code, usage.Body)
	}
	var usageBody struct {
		Identity dopUsageIdentityProof `json:"dop_identity"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &usageBody); err != nil || usageBody.Identity.Subject != "exact-subject" || usageBody.Identity.ProviderID != "dop-oidc" || usageBody.Identity.TeamID != "dop-team" || usageBody.Identity.Role != "user" {
		t.Fatal("usage did not carry the current managed ownership proof")
	}
	catalog := dopNativeRequest(t, handler, "GET", "/api/admin/users", "", nil, nil)
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), "exact-subject") || strings.Contains(catalog.Body.String(), dopOAuthOnlyPassword) || strings.Contains(catalog.Body.String(), "password_hash") {
		t.Fatal("catalog contract or credential filtering failed")
	}
	team := dopNativeRequest(t, handler, "GET", "/api/admin/resources/budgets?dop_team_id=dop-team", "", nil, nil)
	if team.Code != 200 || !strings.Contains(team.Body.String(), "used_usd") || !strings.Contains(team.Body.String(), "UTC") {
		t.Fatalf("team usage: %d %s", team.Code, team.Body)
	}
}

func TestDOPNativeSingleUnsafeInputsAndCompatibility(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopProvisionFixture(t, store, "unused")
	body := map[string]any{"name": "Budget", "status": "active", "fields": map[string]any{"scope": "team", "scope_id": "dop-team", "amount_usd": 10, "enforcement": "hard"}}
	if got := dopNativeRequest(t, handler, "POST", "/api/admin/resources/budgets", "", body, nil); got.Code != 400 {
		t.Fatal("missing operation identity accepted")
	}
	first := dopNativeRequest(t, handler, "POST", "/api/admin/resources/budgets", "budget", body, nil)
	if first.Code != 201 {
		t.Fatalf("budget: %d %s", first.Code, first.Body)
	}
	body["name"] = "Changed"
	if got := dopNativeRequest(t, handler, "POST", "/api/admin/resources/budgets", "budget", body, nil); got.Code != 409 {
		t.Fatal("changed body accepted for original operation")
	}
	r := httptest.NewRequest("GET", "/api/admin/dop/capabilities", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("anonymous capability accepted")
	}
	encoded, _ := json.Marshal(map[string]any{"name": "Native Team", "status": "active"})
	r = httptest.NewRequest("POST", "/api/admin/resources/teams", bytes.NewReader(encoded))
	r.Header.Set("Authorization", "Bearer dev_admin_token")
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 201 || w.Header().Get("X-DOP-Replayed") != "" {
		t.Fatalf("native behavior changed: %d %s", w.Code, w.Body)
	}
}

func TestDOPManagedStatusBatchAndManualChangeFence(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "employee", dopTestItems(dopProvisionFixture(t, store, "employee"))))[0]
	var employee dopProvisionTestResult
	if err := json.Unmarshal(result.Body, &employee); err != nil || employee.User.ID == "" {
		t.Fatalf("provision: %s", result.Body)
	}
	id := employee.User.ID
	disable := dopBatchResults(t, dopBatchRequest(t, handler, "users/status", "disable", dopTestItems(dopTestItem("employee", "", id, map[string]any{"status": "disabled"}))))[0]
	if disable.Status != 200 {
		t.Fatalf("batch disable: %s", disable.Body)
	}
	if err := store.db.Model(&AdminUser{}).Where("id = ?", id).Update("name", "Manually changed").Error; err != nil {
		t.Fatal(err)
	}
	restore := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/"+id, "restore", map[string]any{"status": "active"}, nil)
	if restore.Code != 409 {
		t.Fatalf("manual change overwritten: %d %s", restore.Code, restore.Body)
	}
}

func TestDOPNativeIdentityConversionAndScope(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopProvisionFixture(t, store, "unused")
	legacy, err := store.CreateAdminUser(AdminUser{Username: "legacy", Email: "legacy@example.test", Name: "Legacy", TeamID: "dop-team", Role: "user", Status: StatusActive}, "legacy-password-for-test")
	if err != nil {
		t.Fatal(err)
	}
	session := AdminSession{Token: "legacy-session-for-test", UserID: legacy.ID, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
	if err = store.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	forbidden := dopNativeRequest(t, handler, "GET", "/api/admin/users", "", nil, map[string]string{"Authorization": "Bearer " + session.Token})
	if forbidden.Code != 403 {
		t.Fatalf("employee read admin identity proofs: %d", forbidden.Code)
	}
	headers := map[string]string{"X-DOP-OAuth-Provider": "dop-oidc", "X-DOP-OAuth-Subject": base64.RawURLEncoding.EncodeToString([]byte("legacy-exact-subject"))}
	converted := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/"+legacy.ID, "convert-legacy", map[string]any{}, headers)
	if converted.Code != 200 {
		t.Fatalf("conversion: %d %s", converted.Code, converted.Body)
	}
	var stored AdminUser
	if err = store.db.First(&stored, "id = ?", legacy.ID).Error; err != nil || stored.PasswordHash != dopOAuthOnlyPassword || stored.TeamID != "dop-team" || stored.Status != StatusActive {
		t.Fatal("conversion changed ownership/status or retained password")
	}
	var sessions int64
	if err = store.db.Model(&AdminSession{}).Where("user_id = ?", legacy.ID).Count(&sessions).Error; err != nil || sessions != 0 {
		t.Fatal("legacy session survived conversion")
	}
	headers["X-DOP-OAuth-Subject"] = base64.RawURLEncoding.EncodeToString([]byte("other-subject"))
	immutable := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/"+legacy.ID, "change-binding", map[string]any{}, headers)
	if immutable.Code != 409 {
		t.Fatal("binding was overwritten")
	}
	protected := dopNativeRequest(t, handler, "PATCH", "/api/admin/users/dop-admin", "convert-admin", map[string]any{}, headers)
	if protected.Code != 403 {
		t.Fatal("administrator conversion accepted")
	}
}
