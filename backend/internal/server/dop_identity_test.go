package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestDOPIdentityPasswordGuardsAndAdminCompatibility(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	item := dopProvisionFixture(t, store, "employee")
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "identity", dopTestItems(item)))[0]
	var value dopProvisionTestResult
	if result.Status != 201 || json.Unmarshal(result.Body, &value) != nil {
		t.Fatalf("provision failed: %s", result.Body)
	}
	for _, suffix := range []string{"", "/"} {
		response := doJSON(t, handler, http.MethodPatch, "/api/admin/users/"+value.User.ID+suffix, map[string]any{"password": "shared-password"}, "")
		if response.Code != http.StatusForbidden {
			t.Fatalf("password change bypassed OAuth-only policy: %d %s", response.Code, response.Body)
		}
	}
	login := doJSON(t, handler, http.MethodPost, "/api/admin/auth/login", map[string]any{"identity": "employee@example.test", "password": dopOAuthOnlyPassword}, "")
	if login.Code != http.StatusUnauthorized {
		t.Fatal("OAuth-only employee logged in with password")
	}
	guard := &dopIdentityStore{GormStore: store}
	if _, _, err := guard.CreateAdminPasswordResetToken(value.User.ID, "dop-admin", time.Hour); AsHTTPError(err).Status != 403 {
		t.Fatal("password reset token was issued")
	}
	// Even an old/reset token created outside the guarded HTTP path cannot turn
	// the account into a local-password identity.
	token, _, err := store.CreateAdminPasswordResetToken(value.User.ID, "dop-admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reset := doJSON(t, handler, http.MethodPost, "/api/admin/auth/reset-password", map[string]any{"token": token, "password": "shared-password"}, "")
	if reset.Code != 403 {
		t.Fatalf("reset bypassed OAuth-only policy: %d %s", reset.Code, reset.Body)
	}
	admin, err := store.CreateAdminUser(AdminUser{Username: "local-admin", Email: "local-admin@example.test", Role: "admin", Status: StatusActive}, "original-password")
	if err != nil {
		t.Fatal(err)
	}
	login = doJSON(t, handler, http.MethodPost, "/api/admin/auth/login", map[string]any{"identity": admin.Username, "password": "original-password"}, "")
	if login.Code != 200 {
		t.Fatalf("normal admin login changed: %d %s", login.Code, login.Body)
	}
	change := doJSON(t, handler, http.MethodPatch, "/api/admin/users/"+admin.ID, map[string]any{"password": "replacement-password"}, "")
	if change.Code != 200 {
		t.Fatal("normal administrator password updates were blocked")
	}
}

func TestDOPIdentityBindingPreventsEmailTakeoverAndClaimOverrides(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	item := dopProvisionFixture(t, store, "employee")
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "binding", dopTestItems(item)))[0]
	var value dopProvisionTestResult
	if result.Status != 201 || json.Unmarshal(result.Body, &value) != nil {
		t.Fatal("provision failed")
	}
	api := &dopBatchAPI{server: app, store: store}
	provider, _ := app.findActiveOAuthIdentityProvider("dop-oidc")
	for _, claims := range []map[string]any{
		{"sub": "wrong", "email": "employee@example.test", "email_verified": true},
		{"email": "employee@example.test", "email_verified": true},
		{"sub": "employee", "email": "employee@example.test", "email_verified": false},
	} {
		if _, err := api.resolveOAuthUser(provider, claims); err == nil {
			t.Fatal("unbound or unverified OAuth identity took over employee")
		}
	}
	wrongProvider := provider
	wrongProvider.ID = "different-provider"
	if _, err := api.resolveOAuthUser(wrongProvider, map[string]any{"sub": "employee", "email": "employee@example.test"}); err == nil {
		t.Fatal("other provider took over employee")
	}
	user, err := api.resolveOAuthUser(provider, map[string]any{"sub": "employee", "email": "changed@example.test", "email_verified": true, "name": "Override", "role": "admin", "team": "different-team"})
	if err != nil || user.ID != value.User.ID || user.Name != "Employee" || user.Role != "user" || user.TeamID != "dop-team" || user.Email != "employee@example.test" || len(store.ListAdminUsers()) != 2 {
		t.Fatalf("stable identity changed DOP ownership/profile: %v", err)
	}
	if _, err := store.UpdateAdminUser(user.ID, AdminUser{Status: StatusDisabled}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := api.resolveOAuthUser(provider, map[string]any{"sub": "employee"}); err == nil {
		t.Fatal("disabled employee authenticated")
	}
}
