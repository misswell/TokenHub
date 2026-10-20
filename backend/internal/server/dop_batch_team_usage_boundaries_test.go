package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestDOPBatchTeamUsageValidationAndOptIn(t *testing.T) {
	_, app, handler := newDOPBatchTestServer(t)
	for _, ids := range [][]string{nil, {}, {""}, {" "}, {"a", "a"}, {strings.Repeat("a", 257)}, make([]string, dopBatchLimit+1)} {
		response := dopBatchRequest(t, handler, "teams/usage", "", map[string]any{"team_ids": ids})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid Team IDs accepted: %d", response.Code)
		}
	}
	path := "/api/admin/dop/batch/teams/usage"
	body := map[string]any{"team_ids": []string{"missing"}}
	if response := doJSON(t, handler, http.MethodPost, path, body, "invalid-token"); response.Code != http.StatusUnauthorized {
		t.Fatal("invalid authentication accepted")
	}
	if response := doJSON(t, handler, http.MethodGet, path, nil, ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatal("unexpected HTTP method accepted")
	}
	if response := doJSON(t, app.Handler(), http.MethodPost, path, body, ""); response.Code != http.StatusNotFound {
		t.Fatal("original entrypoint unexpectedly enabled Team batch queries")
	}
}

func TestDOPBatchTeamUsageScalesWithConstantQueries(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	at, err := store.databaseNow(store.db)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, dopBatchLimit)
	teams := make([]AdminResource, len(ids))
	projects := make([]Project, len(ids))
	budgets := make([]AdminResource, len(ids))
	usage := make([]UsageRecord, len(ids))
	for i := range ids {
		id := fmt.Sprintf("team-%05d", i)
		ids[i] = id
		teams[i] = AdminResource{ID: id, Kind: "teams", Name: id, Status: StatusActive}
		projects[i] = Project{ID: "p-" + id, TeamID: id, Status: StatusActive}
		budgets[i] = AdminResource{ID: "b-" + id, Kind: "budgets", Name: id, Status: StatusActive, Fields: map[string]any{"scope": "team", "scope_id": id, "amount_usd": 100}}
		usage[i] = UsageRecord{ID: "u-" + id, RequestID: "r-" + id, ProjectID: projects[i].ID, CostUSD: 20, CreatedAt: at}
	}
	for _, records := range []any{&teams, &projects, &budgets, &usage} {
		if err := store.db.CreateInBatches(records, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, count := range []int{1, 1000, dopBatchLimit} {
		queries := countStoreStatements(t, store, func() {
			data, _, err := store.readDOPBatchTeamUsage(context.Background(), ids[:count])
			if err != nil || len(data) != count {
				t.Fatalf("Team usage query failed for %d Teams: %v", count, err)
			}
			for i, item := range data {
				if item.TeamID != ids[i] || item.UsedUSD == nil || *item.UsedUSD != 20 || item.MonthlyLimitUSD == nil || *item.MonthlyLimitUSD != 100 || item.RemainingUSD == nil || *item.RemainingUSD != 80 {
					t.Fatalf("nonzero Team usage lost for %s", ids[i])
				}
			}
		})
		if queries != 4 {
			t.Fatalf("expected 4 Team aggregate queries for %d Teams, got %d", count, queries)
		}
	}
	queries := countStoreStatements(t, store, func() {
		response := dopBatchRequest(t, handler, "teams/usage", "", map[string]any{"team_ids": ids})
		var decoded struct {
			Data []dopBatchTeamUsage `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || response.Code != http.StatusOK || len(decoded.Data) != len(ids) {
			t.Fatalf("HTTP batch query failed: %d", response.Code)
		}
	})
	if queries != 5 {
		t.Fatalf("expected 5 HTTP queries (4 Team aggregation plus admin authentication), got %d", queries)
	}
}

func TestDOPBatchTeamUsageFailsClosed(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	if err := store.db.Callback().Row().Before("gorm:row").Register("dop_team_usage_failure", func(tx *gorm.DB) {
		if tx.Statement.TableExpr != nil && tx.Statement.TableExpr.SQL == "usage_records AS u" {
			_ = tx.AddError(fmt.Errorf("injected Team usage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Callback().Row().Remove("dop_team_usage_failure"); err != nil {
			t.Error(err)
		}
	})
	response := dopBatchRequest(t, handler, "teams/usage", "", map[string]any{"team_ids": []string{"missing"}})
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "used_usd") || strings.Contains(response.Body.String(), "injected Team usage failure") {
		t.Fatal("failed aggregate was exposed or reported as successful zero usage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.readDOPBatchTeamUsage(ctx, []string{"missing"}); err == nil {
		t.Fatal("cancelled query returned zero usage")
	}
}
