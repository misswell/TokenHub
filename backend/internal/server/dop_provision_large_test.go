package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDOPProvisionTwelveThousand(t *testing.T) {
	store, err := NewSQLiteStoreWithConfig(filepath.Join(t.TempDir(), "provision.db"), Config{SecretKey: "synthetic-provision-encryption-key"})
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
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := dopProvisionFixture(t, store, "template")
	var body dopProvisionRequest
	if err := json.Unmarshal(fixture.Body, &body); err != nil {
		t.Fatal(err)
	}
	items := make([]dopBatchItem, dopBatchLimit)
	for index := range items {
		ref := fmt.Sprintf("employee-%05d", index)
		body.Username, body.Email, body.OAuthSubject = ref, ref+"@example.test", ref
		items[index] = dopTestItem(ref, "", "", body)
	}
	encoded, _ := json.Marshal(dopTestItems(items...))
	var fullUserReads atomic.Int64
	if err := store.db.Callback().Query().After("gorm:query").Register("dop_provision_count_users", func(db *gorm.DB) {
		sql := db.Statement.SQL.String()
		if strings.Contains(sql, "FROM `admin_users`") && !strings.Contains(sql, "WHERE") {
			fullUserReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := dopBatchRequest(t, handler, "users/provision", "large-real-provision", dopTestItems(items...))
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("12000-person provision failed: %d %.1200s", response.Code, response.Body.String())
	}
	reads := fullUserReads.Load()
	if reads > 2 {
		t.Fatalf("provision repeatedly scanned all users: %d", reads)
	}
	results := dopBatchResults(t, response)
	if len(results) != dopBatchLimit {
		t.Fatal("employees missing from batch response")
	}
	for index, result := range results {
		var value dopProvisionTestResult
		if json.Unmarshal(result.Body, &value) != nil || result.ClientRef != items[index].ClientRef || result.Status != 201 || value.Stage != "ready" || value.Key.Secret == "" || value.User.Role != "user" || stringField(value.Quota.Fields, "scope_id") != value.User.ID {
			t.Fatalf("incorrect employee provision at %d", index)
		}
	}
	for _, model := range []any{&dopOAuthIdentity{}, &APIKey{}} {
		var count int64
		if err := store.db.Model(model).Count(&count).Error; err != nil || count != dopBatchLimit {
			t.Fatalf("incorrect persisted resource count: %d %v", count, err)
		}
	}
	var users int64
	if err := store.db.Model(&AdminUser{}).Where("password_hash = ? AND role = ? AND status = ?", dopOAuthOnlyPassword, "user", StatusActive).Count(&users).Error; err != nil || users != dopBatchLimit || len(store.ListResources("quota-policies")) != dopBatchLimit {
		t.Fatal("not all employee identities and quotas were persisted")
	}
	var mode string
	var synchronous int
	store.db.Raw("PRAGMA journal_mode").Scan(&mode)
	store.db.Raw("PRAGMA synchronous").Scan(&synchronous)
	t.Logf("Provisioned %d real User+quota+Key identities on file SQLite in %s; request=%d bytes; full-user reads=%d; journal_mode=%s; synchronous=%d", len(results), elapsed, len(encoded), reads, mode, synchronous)
}
