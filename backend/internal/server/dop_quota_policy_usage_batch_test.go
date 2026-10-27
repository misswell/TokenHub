package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestQuotaPolicyBatchParityAndBoundedQueries(t *testing.T) {
	store, _, _ := newDOPBatchTestServer(t)
	periods, err := currentQuotaPeriods(store.db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"day", "month"} {
		bucket := periods.Month
		if scope == "day" {
			bucket = periods.Day
		}
		for _, row := range []QuotaBucket{
			{KeyID: "user:legacy", Scope: scope, Bucket: bucket, AttributedUserID: "legacy", QuotaCounter: QuotaCounter{Requests: 9}},
			{KeyID: "user:alice", Scope: scope, Bucket: bucket, AttributedUserID: "alice", QuotaCounter: QuotaCounter{Requests: 2, CostUSD: 7}},
			{KeyID: "old-key", Scope: scope, Bucket: bucket, AttributedUserID: "alice", QuotaCounter: QuotaCounter{Requests: 5, CostUSD: 1}},
			{KeyID: "key-a", Scope: scope, Bucket: bucket, AttributedUserID: unattributedQuotaUserID, QuotaCounter: QuotaCounter{Requests: 3}},
		} {
			if err := store.db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
		}
		// Explicit SQL preserves legacy empty attribution despite the model default.
		if err := store.db.Exec("INSERT INTO quota_buckets (key_id,scope,bucket,attributed_user_id,requests,prompt_tokens,completion_tokens,total_tokens,cost_usd) VALUES (?,?,?,?,?,?,?,?,?)", "user:legacy", scope, bucket, "", 4, 0, 0, 0, 0).Error; err != nil {
			t.Fatal(err)
		}
	}
	targets := []dopQuotaPolicyUsageTarget{{"user", "alice"}, {"user", "legacy"}, {"api_key", "key-a"}, {"key", "key-a"}, {"team", "x"}, {"user", allUsersQuotaScopeID}, {"user", ""}}
	for i := 0; i < 2400; i++ {
		targets = append(targets, dopQuotaPolicyUsageTarget{"user", fmt.Sprintf("user-%d", i)}, dopQuotaPolicyUsageTarget{"key", fmt.Sprintf("key-%d", i)})
	}
	queries := 0
	if err := store.db.Callback().Query().Before("gorm:query").Register("count_quota_batch", func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Callback().Row().Before("gorm:row").Register("count_quota_batch", func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.db.Callback().Query().Remove("count_quota_batch")
		_ = store.db.Callback().Row().Remove("count_quota_batch")
	})
	got, err := readDOPQuotaPolicyUsages(context.Background(), store, targets)
	if err != nil {
		t.Fatal(err)
	}
	if queries > 27 {
		t.Fatalf("unbounded query count: %d", queries)
	}
	if got[0].Usage.Daily.Requests != 5 || got[0].Usage.Daily.CostUSD != 7 || got[1].Usage.Daily.Requests != 4 || got[1].Usage.Monthly.Requests != 4 {
		t.Fatalf("unexpected merged or legacy usage: %+v %+v", got[0], got[1])
	}
	for i, target := range targets[:7] {
		want, supported, err := store.GetQuotaPolicyUsage(target.Scope, target.ScopeID)
		if err != nil || got[i].Usage != want || got[i].Supported != supported {
			t.Fatalf("%+v: got %+v want %+v supported %v err %v", target, got[i], want, supported, err)
		}
	}
}

func TestQuotaPolicyBatchErrorsAreSanitized(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	policy := store.CreateResource("quota-policies", AdminResource{Name: "test", Fields: map[string]any{"scope": "user", "scope_id": "alice"}})
	if err := store.db.Callback().Query().Before("gorm:query").Register("quota_batch_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "quota_buckets" {
			_ = tx.AddError(fmt.Errorf("secret database failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Callback().Query().Remove("quota_batch_failure") })
	r := httptest.NewRequest(http.MethodGet, "/api/admin/resources/quota-policies", nil)
	r.Header.Set("Authorization", "Bearer dev_admin_token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret database failure") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		path, header string
		status       int
	}{{"/api/admin/resources/quota-policies?id=" + policy.ID, "", 500}, {"/api/admin/resources/quota-policies", "true", 200}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Authorization", "Bearer dev_admin_token")
		r.Header.Set("X-DOP-Operation", tc.header)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "secret database failure") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDOPQuotaPolicyUsages(ctx, store, []dopQuotaPolicyUsageTarget{{"user", "alice"}}); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestAdminTokenSingleRowAndFailure(t *testing.T) {
	store, app, _ := newDOPBatchTestServer(t)
	dopTestUser(t, store, "later-user")
	var plan []struct{ Detail string }
	if err := store.db.Raw("EXPLAIN QUERY PLAN SELECT * FROM admin_users ORDER BY created_at ASC LIMIT 1").Scan(&plan).Error; err != nil || len(plan) == 0 || !strings.Contains(plan[0].Detail, "idx_dop_admin_users_created_at") {
		t.Fatalf("admin index unavailable: %+v %v", plan, err)
	}
	var rows int64
	var statement string
	if err := store.db.Callback().Query().After("gorm:query").Register("admin_first_count", func(tx *gorm.DB) {
		if tx.Statement.Table == "admin_users" {
			rows = tx.RowsAffected
			statement = tx.Statement.SQL.String()
		}
	}); err != nil {
		t.Fatal(err)
	}
	user, ok, err := firstDOPAdminUser(context.Background(), store)
	if rows != 1 || !strings.Contains(strings.ToUpper(statement), "LIMIT 1") {
		t.Fatalf("query not limited: rows=%d sql=%s", rows, statement)
	}
	_ = store.db.Callback().Query().Remove("admin_first_count")
	if err != nil || !ok || user.ID != store.ListAdminUsers()[0].ID {
		t.Fatalf("wrong user: %+v %v", user, err)
	}

	wrapped := &adminFirstOverride{Store: store}
	wrappedUser, found, err := firstDOPAdminUser(context.Background(), wrapped)
	if err != nil || !found || wrappedUser.ID != "override" {
		t.Fatal("wrapper override ignored")
	}
	if err := store.db.Callback().Query().Before("gorm:query").Register("admin_first_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "admin_users" {
			_ = tx.AddError(fmt.Errorf("secret admin error"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Callback().Query().Remove("admin_first_failure") })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer dev_admin_token")
	w := httptest.NewRecorder()
	api := &dopBatchAPI{server: app, store: store}
	_, ok = api.authorizeDOPAdminUser(w, r)
	if ok || w.Code != 500 || strings.Contains(w.Body.String(), "secret admin error") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
}

type adminFirstOverride struct{ Store }

func (s *adminFirstOverride) ListAdminUsers() []AdminUser { return []AdminUser{{ID: "override"}} }

func TestDOPUserDeleteUsesNativeProtections(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	dopTestUser(t, store, "delete-target")
	for _, user := range []AdminUser{{ID: "leader", Username: "leader", Email: "leader@example.test", Role: "team_leader", TeamID: "team-a", Status: StatusActive}, {ID: "ordinary", Username: "ordinary", Email: "ordinary@example.test", Role: "user", Status: StatusActive}, {ID: "outsider", Username: "outsider", Email: "outsider@example.test", Role: "user", TeamID: "team-b", Status: StatusActive}} {
		if err := store.db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, leaderSession, err := store.CreateAdminSession("leader", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, userSession, err := store.CreateAdminSession("ordinary", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	requestAs := func(id, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+id, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := requestAs("outsider", leaderSession.Token); w.Code != 403 {
		t.Fatalf("cross-team delete: %d %s", w.Code, w.Body.String())
	}
	if w := requestAs("outsider", userSession.Token); w.Code != 403 {
		t.Fatalf("user delete: %d %s", w.Code, w.Body.String())
	}
	auditBefore := len(store.ListAuditEvents())
	var firstQuery bool
	if err := store.db.Callback().Query().After("gorm:query").Register("delete_admin_limit", func(tx *gorm.DB) {
		if tx.Statement.Table == "admin_users" && strings.Contains(strings.ToUpper(tx.Statement.SQL.String()), "LIMIT 1") {
			firstQuery = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Callback().Query().Remove("delete_admin_limit") })
	request := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, "/api/admin/users/"+id, nil)
		r.Header.Set("Authorization", "Bearer dev_admin_token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("dop-admin"); w.Code != http.StatusBadRequest {
		t.Fatalf("self deletion: %d %s", w.Code, w.Body.String())
	}
	if w := request("delete-target"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if !firstQuery || len(store.ListAuditEvents()) <= auditBefore {
		t.Fatal("optimized lookup or audit missing")
	}
	var count int64
	if err := store.db.Model(&AdminUser{}).Where("id = ?", "delete-target").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("user still exists: %d %v", count, err)
	}
}
