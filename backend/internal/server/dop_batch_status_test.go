package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDOPBatchStatusPreservesProjectConfigurationAndRestoreOwnership(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	team := store.CreateResource("teams", AdminResource{Name: "Company"})
	project := store.CreateProject(Project{Name: "Company", TeamID: team.ID, OwnerUserID: "dop-admin", CostCenter: "cost-101", DefaultQuotaRef: "keep-quota", ModelAccessMode: "inherit", Status: StatusActive})
	manual := store.CreateProject(Project{Name: "Manually disabled", Status: StatusDisabled})
	for index, status := range []string{StatusDisabled, StatusActive} {
		item := dopTestItem("project", "", project.ID, map[string]any{"status": status})
		response := dopBatchRequest(t, handler, "projects/status", status, dopTestItems(item))
		if response.Code != http.StatusOK {
			t.Fatalf("project status failed: %s", response.Body)
		}
		updated, ok := store.GetProject(project.ID)
		if !ok || updated.TeamID != project.TeamID || updated.OwnerUserID != project.OwnerUserID || updated.CostCenter != project.CostCenter || updated.DefaultQuotaRef != project.DefaultQuotaRef || updated.Status != status {
			t.Fatalf("status change %d altered project settings: %+v", index, updated)
		}
	}
	for _, status := range []string{StatusDisabled, StatusActive} {
		item := dopTestItem("manual", "", manual.ID, map[string]any{"status": status})
		response := dopBatchRequest(t, handler, "projects/status", "manual-"+status, dopTestItems(item))
		if status == StatusActive && dopBatchResults(t, response)[0].Status != http.StatusConflict {
			t.Fatal("manual disabled project was restored")
		}
	}
}

func TestDOPBatchKeyStatusRejectsExpiredOrChangedKeys(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	project := store.CreateProject(Project{Name: "Company", Status: StatusActive})
	key, _, err := store.CreateAPIKey(project.ID, APIKey{Name: "Employee", Status: StatusActive}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{StatusDisabled, StatusActive} {
		response := dopBatchRequest(t, handler, "api-keys/status", status, dopTestItems(dopTestItem("key", "", key.ID, map[string]any{"status": status})))
		if response.Code != http.StatusOK {
			t.Fatalf("key status failed: %s", response.Body)
		}
	}
	disable := dopTestItem("key", "", key.ID, map[string]any{"status": StatusDisabled})
	if response := dopBatchRequest(t, handler, "api-keys/status", "disable-again", dopTestItems(disable)); response.Code != http.StatusOK {
		t.Fatal(response.Body)
	}
	if _, err := store.UpdateAPIKey(key.ID, APIKey{Name: "Manual change"}); err != nil {
		t.Fatal(err)
	}
	active := dopTestItem("key", "", key.ID, map[string]any{"status": StatusActive})
	if result := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/status", "restore-changed", dopTestItems(active)))[0]; result.Status != http.StatusConflict {
		t.Fatal("changed key was restored")
	}
	expired := time.Now().Add(-time.Hour)
	expiredKey, _, err := store.CreateAPIKey(project.ID, APIKey{Name: "Expired", ExpiresAt: &expired, Status: StatusActive}, "")
	if err != nil {
		t.Fatal(err)
	}
	disable.ID, active.ID = expiredKey.ID, expiredKey.ID
	if response := dopBatchRequest(t, handler, "api-keys/status", "expire-disable", dopTestItems(disable)); response.Code != http.StatusOK {
		t.Fatal(response.Body)
	}
	if result := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/status", "expire-restore", dopTestItems(active)))[0]; result.Status != http.StatusConflict {
		t.Fatal("expired key was restored")
	}
}

func TestDOPBatchRestrictsSessionsToPlatformAdministrators(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	for _, role := range []string{"user", "team_leader", "security_admin"} {
		dopTestUser(t, store, role)
		if err := store.db.Model(&AdminUser{}).Where("id = ?", role).Update("role", role).Error; err != nil {
			t.Fatal(err)
		}
		_, session, err := store.CreateAdminSession(role, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"users/usage", "user-quotas", "team-budgets", "projects/status", "api-keys/status", "api-keys/create"} {
			r := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/"+path, strings.NewReader(`{"items":[]}`))
			r.Header.Set("Authorization", "Bearer "+session.Token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s accessed %s: %d", role, path, w.Code)
			}
		}
	}
}

func TestDOPBatchRestoreRejectsSubsequentManualDisable(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	project := store.CreateProject(Project{Name: "Company", Status: StatusActive})
	key, _, err := store.CreateAPIKey(project.ID, APIKey{Name: "Employee", Status: StatusActive}, "")
	if err != nil {
		t.Fatal(err)
	}
	disable := dopTestItem("key", "", key.ID, map[string]any{"status": StatusDisabled})
	if response := dopBatchRequest(t, handler, "api-keys/status", "disable", dopTestItems(disable)); response.Code != http.StatusOK {
		t.Fatal(response.Body)
	}
	// A manual disable of an already-disabled Key has the same public object
	// fingerprint. Its newer audit still revokes extension restore ownership.
	if response := doJSON(t, app.Handler(), http.MethodPatch, "/api/admin/api-keys/"+key.ID, map[string]any{"status": StatusDisabled}, ""); response.Code != http.StatusOK {
		t.Fatal(response.Body)
	}
	active := dopTestItem("key", "", key.ID, map[string]any{"status": StatusActive})
	if result := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/status", "restore", dopTestItems(active)))[0]; result.Status != http.StatusConflict {
		t.Fatal("manual no-op disable was overridden")
	}
}
