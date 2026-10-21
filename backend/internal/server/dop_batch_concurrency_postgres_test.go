package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

// Use only a dedicated local PostgreSQL test instance. Every test gets its own
// database; no existing schema or shared-server data is modified.
func newDOPPostgresTestServer(t *testing.T, options ...DOPBatchOptions) (*GormStore, *Server, http.Handler, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DOP_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_DOP_POSTGRES_URL is not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid local test database configuration")
	}
	if config.Host != "127.0.0.1" && config.Host != "localhost" && config.Host != "::1" {
		t.Fatal("DOP concurrency tests require a local isolated PostgreSQL instance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot connect to local test PostgreSQL")
	}
	database := NewID("dop_pool")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error("failed to drop isolated DOP test database")
		}
		_ = admin.Close(cleanup)
	})
	user := url.User(config.User)
	if config.Password != "" {
		user = url.UserPassword(config.User, config.Password)
	}
	u := url.URL{Scheme: "postgres", User: user, Host: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), Path: "/" + database, RawQuery: "sslmode=disable"}
	testDSN := u.String()
	store, err := NewStoreWithDialect(testDSN, Config{Environment: "test", DatabaseURL: testDSN, SecretKey: "synthetic-dop-pool-test-secret-key", DBMaxOpenConns: 25, DBMaxIdleConns: 8})
	if err != nil {
		t.Fatal("cannot initialize isolated DOP test store")
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.db.Create(&AdminUser{ID: "dop-admin", Username: "dop-admin", Email: "admin@example.test", Role: "admin", Status: StatusActive}).Error; err != nil {
		t.Fatal(err)
	}
	app := New(store)
	t.Cleanup(func() {
		if err := app.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var configured DOPBatchOptions
	if len(options) > 0 {
		configured = options[0]
	}
	handler, err := app.DOPBatchHandlerWithOptions(configured)
	if err != nil {
		t.Fatal(err)
	}
	return store, app, handler, testDSN
}

func dopConcurrentHTTP(t *testing.T, handler http.Handler, path, key string, items []dopBatchItem, workers int) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body := dopTestItems(items...)
	if workers != 0 {
		body["concurrency"] = workers
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/"+path, bytes.NewReader(encoded))
		r.Header.Set("Authorization", "Bearer dev_admin_token")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		done <- w
	}()
	return done
}

func dopWaitHTTP(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(30 * time.Second):
		t.Fatal("DOP HTTP worker did not finish")
		return nil
	}
}

func dopProvisionItems(t *testing.T, store *GormStore, prefix string, count int) []dopBatchItem {
	t.Helper()
	fixture := dopProvisionFixture(t, store, prefix)
	var body dopProvisionRequest
	if err := json.Unmarshal(fixture.Body, &body); err != nil {
		t.Fatal(err)
	}
	items := make([]dopBatchItem, count)
	for i := range items {
		ref := fmt.Sprintf("%s-%04d", prefix, i)
		body.Username, body.Email, body.OAuthSubject = ref, ref+"@example.test", ref
		items[i] = dopTestItem(ref, "", "", body)
	}
	return items
}

func TestDOPPostgresSameTeamOverlapAndAdmission(t *testing.T) {
	store, _, handler, _ := newDOPPostgresTestServer(t)
	first := dopProvisionItems(t, store, "first", 4)
	second := dopProvisionItems(t, store, "second", 4)
	third := dopProvisionItems(t, store, "third", 1)
	entered := make(chan struct{}, 8)
	barrier := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(barrier) }) })
	name := "dop_pg_overlap"
	if err := store.db.Callback().Create().After("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "dop_oauth_identities" {
			select {
			case entered <- struct{}{}:
			case <-tx.Statement.Context.Done():
				return
			}
			select {
			case <-barrier:
			case <-tx.Statement.Context.Done():
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	doneA := dopConcurrentHTTP(t, handler, "users/provision", "first", first, 4)
	doneB := dopConcurrentHTTP(t, handler, "users/provision", "second", second, 4)
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	overlapped := true
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-deadline.C:
			overlapped = false
			i = 8
		}
	}
	if overlapped {
		response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "third", third, 4))
		if response.Code != 429 || response.Header().Get("Retry-After") != "2" {
			t.Error("third concurrent request was not rejected before writes")
		}
	}
	release.Do(func() { close(barrier) })
	for _, done := range []<-chan *httptest.ResponseRecorder{doneA, doneB} {
		if response := dopWaitHTTP(t, done); response.Code != 200 {
			t.Errorf("parallel provisioning failed: HTTP %d", response.Code)
		}
	}
	if err := store.db.Callback().Create().Remove(name); err != nil {
		t.Fatal(err)
	}
	if !overlapped {
		t.Fatal("same-Team users did not overlap while holding Team/Project protection")
	}
	var count int64
	if err := store.db.Model(&AdminUser{}).Where("username LIKE ?", "third-%").Count(&count).Error; err != nil || count != 0 {
		t.Fatal("rejected request left users")
	}
	if response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "third", third, 4)); response.Code != 200 {
		t.Fatal("admission was not released")
	}
}

func TestDOPPostgresApprovalsRollbackAndOrderedUpdates(t *testing.T) {
	store, _, handler, _ := newDOPPostgresTestServer(t)
	items := dopProvisionItems(t, store, "pending", 4)
	flow := store.CreateResource("approval-flows", AdminResource{Name: "Required", Status: StatusActive, Fields: map[string]any{"trigger": "quota_increase"}})
	response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "pending", items, 4))
	if response.Code != 207 {
		t.Fatalf("pending batch failed: HTTP %d", response.Code)
	}
	for _, result := range dopBatchResults(t, response) {
		var value dopProvisionTestResult
		if json.Unmarshal(result.Body, &value) != nil || result.Status != 202 || value.Stage != "quota_pending" || value.User.Status != StatusDisabled || value.Key.ID != "" {
			t.Fatal("pending approval exposed an active user or Key")
		}
	}
	if len(store.ListProjectKeys("dop-project")) != 0 {
		t.Fatal("pending quota issued Keys")
	}
	if err := store.DeleteResource("approval-flows", flow.ID); err != nil {
		t.Fatal(err)
	}
	items = dopProvisionItems(t, store, "rollback", 3)
	var body dopProvisionRequest
	_ = json.Unmarshal(items[1].Body, &body)
	body.Key = json.RawMessage(`{"name":"Invalid","model_access_mode":"invalid"}`)
	items[1] = dopTestItem(items[1].ClientRef, "", "", body)
	response = dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "rollback", items, 4))
	results := dopBatchResults(t, response)
	if response.Code != 207 || results[0].State != "succeeded" || results[1].State != "failed" || results[2].State != "succeeded" {
		t.Fatal("partial failures changed ordering or atomicity")
	}
	var value dopProvisionTestResult
	_ = json.Unmarshal(results[0].Body, &value)
	updates := []dopBatchItem{dopTestItem("first", "PATCH", value.Quota.ID, AdminResource{Fields: map[string]any{"monthly_cost_usd": 30}}), dopTestItem("second", "PATCH", value.Quota.ID, AdminResource{Fields: map[string]any{"monthly_cost_usd": 60}})}
	if response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "user-quotas", "ordered", updates, 4)); response.Code != 200 {
		t.Fatal("ordered updates failed")
	}
	var quota AdminResource
	if err := store.db.Where("kind = ? AND id = ?", "quota-policies", value.Quota.ID).First(&quota).Error; err != nil || float64Field(quota.Fields, "monthly_cost_usd") != 60 {
		t.Fatal("same-resource input order was lost")
	}
	var failedUsers int64
	_ = store.db.Model(&AdminUser{}).Where("username = ?", body.Username).Count(&failedUsers).Error
	if failedUsers != 0 {
		t.Fatal("failed employee left a partial account")
	}
}

func TestDOPPostgresCrossInstanceReplayAndLegacyDefault(t *testing.T) {
	store, _, handler, dsn := newDOPPostgresTestServer(t)
	other, err := NewStoreWithDialect(dsn, Config{Environment: "test", DatabaseURL: dsn, SecretKey: "synthetic-dop-pool-test-secret-key", DBMaxOpenConns: 25})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	app := New(other)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	secondHandler, err := app.DOPBatchHandler()
	if err != nil {
		t.Fatal(err)
	}
	items := dopProvisionItems(t, store, "replay", 4)
	first := dopConcurrentHTTP(t, handler, "users/provision", "same-operation", items, 4)
	second := dopConcurrentHTTP(t, secondHandler, "users/provision", "same-operation", items, 4)
	a, b := dopWaitHTTP(t, first), dopWaitHTTP(t, second)
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("concurrent replay failed: %d/%d", a.Code, b.Code)
	}
	ra, rb := dopBatchResults(t, a), dopBatchResults(t, b)
	for i := range ra {
		if !bytes.Equal(ra[i].Body, rb[i].Body) || ra[i].Replayed == rb[i].Replayed {
			t.Fatal("cross-instance replay duplicated or changed a result")
		}
	}
	legacy := dopProvisionItems(t, store, "legacy", 2)
	response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "legacy", legacy, 0))
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(response.Body.Bytes(), &envelope)
	if response.Code != 200 || envelope["execution"] != nil {
		t.Fatal("omitted concurrency changed legacy response")
	}
	var count int64
	_ = store.db.Model(&AdminUser{}).Where("username LIKE ?", "replay-%").Count(&count).Error
	if count != 4 {
		t.Fatal("idempotency created extra users")
	}
}
