package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDOPBatchUsageMatchesNativeCounters(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	for _, id := range []string{"employee-a", "employee-b", "employee-zero"} {
		dopTestUser(t, store, id)
	}
	store.CreateResource("settings", AdminResource{ID: gatewaySettingsID, Name: "Gateway", Fields: map[string]any{quotaTimezoneField: "Asia/Shanghai"}})
	periods, err := currentQuotaPeriods(store.db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"day", "month"} {
		bucket := periods.Month
		if scope == "day" {
			bucket = periods.Day
		}
		rows := []QuotaBucket{
			{KeyID: "user:employee-a", Scope: scope, Bucket: bucket, AttributedUserID: "employee-a", QuotaCounter: QuotaCounter{CostUSD: 5, Requests: 1}},
			{KeyID: "old-deleted-key", Scope: scope, Bucket: bucket, AttributedUserID: "employee-a", QuotaCounter: QuotaCounter{CostUSD: 2, Requests: 2}},
			{KeyID: "reassigned-key", Scope: scope, Bucket: bucket, AttributedUserID: "employee-a", QuotaCounter: QuotaCounter{CostUSD: 7, Requests: 3}},
			{KeyID: "user:employee-b", Scope: scope, Bucket: bucket, AttributedUserID: "employee-b", QuotaCounter: QuotaCounter{CostUSD: 20, Requests: 10}},
			{KeyID: "employee-b-key", Scope: scope, Bucket: bucket, AttributedUserID: "employee-b", QuotaCounter: QuotaCounter{CostUSD: 8, Requests: 4}},
			{KeyID: "history", Scope: scope, Bucket: "1999-01", AttributedUserID: "employee-a", QuotaCounter: QuotaCounter{CostUSD: 9999}},
		}
		if err := store.db.Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	limitA, limitB := 10.0, 5.0
	inputs := []dopBatchUsageInput{{UserID: "employee-b", MonthlyLimitUSD: &limitB}, {UserID: "employee-a", MonthlyLimitUSD: &limitA}, {UserID: "employee-zero"}, {UserID: "missing"}}
	response := dopBatchRequest(t, handler, "users/usage", "", map[string]any{"users": inputs})
	var decoded struct {
		Data     []dopBatchUserUsage `json:"data"`
		Periods  quotaPeriods        `json:"periods"`
		Currency string              `json:"currency"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || response.Code != http.StatusMultiStatus || len(decoded.Data) != len(inputs) {
		t.Fatalf("usage failed: %d %s", response.Code, response.Body)
	}
	if decoded.Currency != "USD" || decoded.Periods != periods {
		t.Fatal("native quota timezone or currency was lost")
	}
	for index, input := range inputs[:3] {
		actual := decoded.Data[index]
		expected, supported, err := store.GetQuotaPolicyUsage("user", input.UserID)
		if err != nil || !supported || actual.CurrentUsage != expected || actual.UserID != input.UserID {
			t.Fatalf("native counter mismatch for %s: %+v != %+v", input.UserID, actual.CurrentUsage, expected)
		}
	}
	if *decoded.Data[0].RemainingUSD != 0 || *decoded.Data[1].RemainingUSD != 1 || decoded.Data[2].RemainingUSD != nil || decoded.Data[3].Status != http.StatusNotFound {
		t.Fatal("remaining calculation, optional target or missing-user handling is incorrect")
	}
}

func TestDOPBatchUsageUsesConstantQueryCountAndFailsClosed(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	users := make([]AdminUser, dopBatchLimit)
	inputs := make([]dopBatchUsageInput, len(users))
	for index := range users {
		id := fmt.Sprintf("employee-%04d", index)
		users[index] = AdminUser{ID: id, Username: id, Email: id + "@example.test", Role: "user", Status: StatusActive}
		inputs[index] = dopBatchUsageInput{UserID: id}
	}
	if err := store.db.CreateInBatches(&users, 100).Error; err != nil {
		t.Fatal(err)
	}
	queries := 0
	callback := "dop_count_queries"
	if err := store.db.Callback().Query().Before("gorm:query").Register(callback, func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Callback().Row().Before("gorm:row").Register(callback, func(*gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Callback().Query().Remove(callback); err != nil {
			t.Error(err)
		}
		if err := store.db.Callback().Row().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	for _, count := range []int{1, 1000, dopBatchLimit} {
		queries = 0
		response := dopBatchRequest(t, handler, "users/usage", "", map[string]any{"users": inputs[:count]})
		if response.Code != http.StatusOK || queries > 9 {
			t.Fatalf("expected bounded query count for %d users, got %d queries: %d", count, queries, response.Code)
		}
	}
	if err := store.db.Callback().Query().Before("gorm:query").Register("dop_usage_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "quota_buckets" {
			_ = tx.AddError(fmt.Errorf("injected quota read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Callback().Query().Remove("dop_usage_failure"); err != nil {
			t.Error(err)
		}
	})
	response := dopBatchRequest(t, handler, "users/usage", "", map[string]any{"users": inputs[:1]})
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "current_usage") || strings.Contains(response.Body.String(), "injected quota read failure") {
		t.Fatal("database failure was exposed or misreported as zero usage")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := store.readDOPBatchUsage(cancelled, inputs[:1]); err == nil {
		t.Fatal("cancelled usage query returned successful zero counters")
	}
}

func TestDOPBatchRejectsInvalidInputBeforeWriting(t *testing.T) {
	_, _, handler := newDOPBatchTestServer(t)
	item := dopTestItem("same", http.MethodPost, "", map[string]any{"name": "Quota"})
	for _, test := range []struct {
		name, route, key string
		body             any
	}{
		{"missing idempotency", "user-quotas", "", dopTestItems(item)},
		{"duplicate refs", "user-quotas", "k", dopTestItems(item, item)},
		{"empty batch", "user-quotas", "k", dopTestItems()},
		{"too many items", "user-quotas", "k", dopTestItems(make([]dopBatchItem, dopBatchLimit+1)...)},
		{"array body", "user-quotas", "k", dopTestItems(dopTestItem("one", "POST", "", []string{"bad"}))},
		{"duplicate users", "users/usage", "", map[string]any{"users": []dopBatchUsageInput{{UserID: "same"}, {UserID: "same"}}}},
		{"no users", "users/usage", "", map[string]any{"users": []any{}}},
		{"too many users", "users/usage", "", map[string]any{"users": make([]dopBatchUsageInput, dopBatchLimit+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if response := dopBatchRequest(t, handler, test.route, test.key, test.body); response.Code != http.StatusBadRequest {
				t.Fatalf("invalid input accepted: %d %s", response.Code, response.Body)
			}
		})
	}
}
