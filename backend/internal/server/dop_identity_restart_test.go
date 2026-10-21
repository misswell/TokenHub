package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestDOPIdentityBindingsAndGuardsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "employees.db")
	config := Config{SecretKey: "synthetic-identity-restart-key"}
	store, err := NewSQLiteStoreWithConfig(path, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&AdminUser{ID: "dop-admin", Username: "dop-admin", Email: "admin@example.test", Role: "admin", Status: StatusActive, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	app := New(store)
	handler, err := app.DOPBatchHandler()
	if err != nil {
		t.Fatal(err)
	}
	item := dopProvisionFixture(t, store, "employee")
	first := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "restart-employee", dopTestItems(item)))[0]
	var value dopProvisionTestResult
	if first.Status != 201 || json.Unmarshal(first.Body, &value) != nil {
		t.Fatalf("provision failed: %s", first.Body)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStoreWithConfig(path, config)
	if err != nil {
		t.Fatal(err)
	}
	app = New(store)
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	handler, err = app.DOPBatchHandler()
	if err != nil {
		t.Fatal(err)
	}
	second := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "restart-employee", dopTestItems(item)))[0]
	if !second.Replayed || string(first.Body) != string(second.Body) {
		t.Fatal("restart lost identity/Key receipt")
	}
	api := &dopBatchAPI{server: app, store: store}
	provider, _ := app.findActiveOAuthIdentityProvider("dop-oidc")
	user, err := api.resolveOAuthUser(provider, map[string]any{"sub": "employee"})
	if err != nil || user.ID != value.User.ID {
		t.Fatal("restart lost provider/subject binding")
	}
	// A model-only restore could omit extension metadata. The credential marker
	// must still block local password setup and native email-based OAuth linking.
	if err := store.db.Delete(&dopOAuthIdentity{}, "user_id = ?", value.User.ID).Error; err != nil {
		t.Fatal(err)
	}
	change := doJSON(t, handler, http.MethodPatch, "/api/admin/users/"+value.User.ID, map[string]any{"password": "shared-password"}, "")
	if change.Code != 403 {
		t.Fatal("missing metadata enabled password setup")
	}
	if _, err := api.resolveOAuthUser(provider, map[string]any{"sub": "employee", "email": "employee@example.test"}); err == nil {
		t.Fatal("missing metadata allowed native email-based account linking")
	}
}
