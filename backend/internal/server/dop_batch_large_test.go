package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDOPBatchTwentyThousandQuotaWrites(t *testing.T) {
	store, app, handler := newDOPBatchTestServer(t)
	users := make([]AdminUser, dopBatchLimit)
	items := make([]dopBatchItem, len(users))
	for index := range users {
		id := fmt.Sprintf("large-employee-%05d", index)
		users[index] = AdminUser{ID: id, Username: id, Email: id + "@example.test", Role: "user", Status: StatusActive}
		items[index] = dopTestItem(id, http.MethodPost, "", AdminResource{Name: id, Description: "Preserved", Fields: map[string]any{"scope": "user", "scope_id": id, "monthly_cost_usd": 50, "daily_requests": 100}})
	}
	if err := store.db.CreateInBatches(&users, 100).Error; err != nil {
		t.Fatal(err)
	}
	periods, err := currentQuotaPeriods(store.db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Create(&QuotaBucket{KeyID: userQuotaBucketKey(users[0].ID), Scope: "month", Bucket: periods.Month, AttributedUserID: users[0].ID, QuotaCounter: QuotaCounter{CostUSD: 7}}).Error; err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	created := dopBatchRequest(t, handler, "user-quotas", "large-create", dopTestItems(items...))
	if created.Code != http.StatusOK {
		t.Fatalf("20000-person creation failed: %d %.1000s", created.Code, created.Body.String())
	}
	results := dopBatchResults(t, created)
	if len(results) != dopBatchLimit {
		t.Fatalf("expected 20000 results, got %d", len(results))
	}
	for index, result := range results {
		var resource AdminResource
		if err := json.Unmarshal(result.Body, &resource); err != nil || result.State != "succeeded" || result.ClientRef != users[index].ID || stringField(resource.Fields, "scope_id") != users[index].ID {
			t.Fatalf("incorrect creation result at index %d", index)
		}
		items[index] = dopTestItem(result.ClientRef, http.MethodPatch, resource.ID, AdminResource{Fields: map[string]any{"monthly_cost_usd": 100}})
	}
	t.Logf("Created %d distinct quotas in %s", len(results), time.Since(started))
	started = time.Now()
	updated := dopBatchRequest(t, handler, "user-quotas", "large-update", dopTestItems(items...))
	if updated.Code != http.StatusOK {
		t.Fatalf("20000-person update failed: %d %.1000s", updated.Code, updated.Body.String())
	}
	results = dopBatchResults(t, updated)
	for index, result := range results {
		var resource AdminResource
		if err := json.Unmarshal(result.Body, &resource); err != nil || result.State != "succeeded" || resource.Description != "Preserved" || float64Field(resource.Fields, "daily_requests") != 100 || float64Field(resource.Fields, "monthly_cost_usd") != 100 {
			t.Fatalf("incorrect update result at index %d", index)
		}
	}
	if len(results) != dopBatchLimit || len(store.ListResources("quota-policies")) != dopBatchLimit {
		t.Fatal("bulk update lost or duplicated policies")
	}
	usage, _, err := store.GetQuotaPolicyUsage("user", users[0].ID)
	if err != nil || usage.Monthly.CostUSD != 7 {
		t.Fatal("bulk adjustment reset monthly spending")
	}
	t.Logf("Updated %d distinct quotas in %s", len(results), time.Since(started))
	// The item limit and byte limit are independent. Larger 20000-Key payloads
	// use the existing configurable byte cap without changing native defaults.
	full := map[string]any{"project_id": "project-001", "owner_user_id": "user-001", "name": "Employee key", "group": "default", "model_access_mode": "inherit", "allowed_models": []string{}, "ip_allowlist": []string{}, "limits": QuotaLimits{}, "rate_limit_rpm": nil, "token_limit_tpm": nil, "expires_at": nil}
	for index := range items {
		items[index] = dopTestItem(users[index].ID, "", "", full)
	}
	encoded, err := json.Marshal(dopTestItems(items...))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Items []dopBatchItem `json:"items"`
	}
	if int64(len(encoded)) > defaultMaxJSONRequestBytes {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/api-keys/create", bytes.NewReader(encoded))
		if err := app.decodeJSON(httptest.NewRecorder(), request, &decoded); err == nil || AsHTTPError(err).Status != http.StatusRequestEntityTooLarge {
			t.Fatal("default JSON byte limit was bypassed")
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/admin/dop/batch/api-keys/create", bytes.NewReader(encoded))
	if err := app.decodeJSONLimit(httptest.NewRecorder(), request, &decoded, 16<<20); err != nil || validateDOPBatchItems(decoded.Items) != nil {
		t.Fatalf("20000 full Key payloads rejected by configured byte or item limit: %v (%d bytes)", err, len(encoded))
	}
}
