package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestDOPBatchReceiptsSurviveStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dop.db")
	config := Config{SecretKey: "synthetic-dop-test-encryption-key"}
	store, err := NewSQLiteStoreWithConfig(path, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&AdminUser{ID: "dop-admin", Username: "dop-admin", Email: "admin@example.test", Role: "admin", Status: StatusActive}).Error; err != nil {
		t.Fatal(err)
	}
	project := store.CreateProject(Project{Name: "Company", Status: StatusActive})
	app := New(store)
	handler, err := app.DOPBatchHandler()
	if err != nil {
		t.Fatal(err)
	}
	item := dopTestItem("employee", "", "", map[string]any{"project_id": project.ID, "owner_user_id": "dop-admin", "name": "Restart-safe key", "model_access_mode": "inherit"})
	response := dopBatchRequest(t, handler, "api-keys/create", "restart-safe", dopTestItems(item))
	first := dopBatchResults(t, response)[0]
	if first.Status != http.StatusCreated {
		t.Fatal(response.Body)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStoreWithConfig(path, config)
	if err != nil {
		t.Fatalf("native startup rejected extension tables: %v", err)
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
	second := dopBatchResults(t, dopBatchRequest(t, handler, "api-keys/create", "restart-safe", dopTestItems(item)))[0]
	if !second.Replayed || !bytes.Equal(first.Body, second.Body) || len(store.ListProjectKeys(project.ID)) != 1 {
		t.Fatal("restart duplicated the Key or lost its encrypted result")
	}
	var payload struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(first.Body, &payload); err != nil || payload.APIKey == "" {
		t.Fatal("secret missing from key response")
	}
	for _, filename := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(filename)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(payload.APIKey)) {
			t.Fatal("cleartext Key leaked into database or WAL")
		}
	}
}
