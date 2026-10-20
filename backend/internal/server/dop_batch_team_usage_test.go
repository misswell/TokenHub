package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func dopTestTeamBudget(t *testing.T, store *GormStore, id, team, period string, amount float64, mode, status string) {
	t.Helper()
	if _, err := store.CreateResourceChecked("budgets", AdminResource{ID: id, Name: id, Status: status, Fields: map[string]any{
		"scope": "team", "scope_id": team, "period_ref": period, "amount_usd": amount, "enforcement": mode,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestDOPBatchTeamUsageMatchesNativeBudgetScopes(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	now, err := store.databaseNow(store.db)
	if err != nil {
		t.Fatal(err)
	}
	period := now.UTC().Format("2006-01")
	start, end := periodStart(period), periodEnd(period)
	for _, id := range []string{"a", "b", "empty", "uncapped"} {
		store.CreateResource("teams", AdminResource{ID: id, Name: id, Status: StatusActive})
	}
	// Personal quota timezone must not change the Team's native UTC month.
	store.CreateResource("settings", AdminResource{ID: gatewaySettingsID, Name: "Gateway", Fields: map[string]any{quotaTimezoneField: "Asia/Shanghai"}})
	projects := []Project{
		{ID: "p-a", TeamID: "a", Status: StatusActive},
		{ID: "p-disabled", TeamID: "a", Status: StatusDisabled},
		{ID: "p-b", TeamID: "b", Status: StatusActive},
		{ID: "p-moved", TeamID: "a", Status: StatusActive},
		{ID: "p-uncapped", TeamID: "uncapped", Status: StatusActive},
	}
	if err := store.db.Create(&projects).Error; err != nil {
		t.Fatal(err)
	}
	for _, usage := range []struct {
		project string
		cost    float64
		at      time.Time
	}{
		{"p-a", 10, start.Add(time.Hour)}, {"p-a", 5, start}, {"p-disabled", 20, start},
		{"p-b", 20, start}, {"p-moved", 7, start}, {"p-uncapped", 9, start},
		{"p-a", 999, start.Add(-time.Nanosecond)}, {"p-a", 999, end}, {"deleted-project", 999, start},
	} {
		addRuntimeBudgetUsage(t, store, usage.project, usage.cost, usage.at)
	}
	if err := store.db.Model(&Project{}).Where("id = ?", "p-moved").Update("team_id", "b").Error; err != nil {
		t.Fatal(err)
	}
	dopTestTeamBudget(t, store, "a-recurring", "a", "", 100, "hard", StatusActive)
	dopTestTeamBudget(t, store, "a-current", "a", period, 80, "", StatusActive)
	dopTestTeamBudget(t, store, "a-monitor", "a", period, 5, "monitor", StatusActive)
	dopTestTeamBudget(t, store, "a-old", "a", start.AddDate(0, -1, 0).Format("2006-01"), 1, "hard", StatusActive)
	dopTestTeamBudget(t, store, "a-disabled", "a", "", 1, "hard", StatusDisabled)
	dopTestTeamBudget(t, store, "b-legacy", "b", "", 10, "hard", StatusActive)
	var legacy AdminResource
	if err := store.db.Where("kind = ? AND id = ?", "budgets", "b-legacy").First(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	delete(legacy.Fields, "scope_id")
	legacy.Fields["team_id"] = "b"
	if err := store.db.Save(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	dopTestTeamBudget(t, store, "uncapped-zero", "uncapped", "", 0, "hard", StatusActive)
	dopTestTeamBudget(t, store, "uncapped-warn", "uncapped", "", 5, "warn", StatusActive)
	store.CreateResource("budgets", AdminResource{Name: "Global", Fields: map[string]any{"scope": "global", "amount_usd": 1}})
	ids := []string{"b", "a", "empty", "uncapped", "missing"}
	response := dopBatchRequest(t, handler, "teams/usage", "", map[string]any{"team_ids": ids})
	var decoded struct {
		Data     []dopBatchTeamUsage `json:"data"`
		Period   string              `json:"period"`
		Timezone string              `json:"timezone"`
		Currency string              `json:"currency"`
		AsOf     time.Time           `json:"as_of"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || response.Code != http.StatusMultiStatus || len(decoded.Data) != len(ids) {
		t.Fatalf("unexpected Team usage response: %d %s", response.Code, response.Body)
	}
	if decoded.Period != period || decoded.Timezone != "UTC" || decoded.Currency != "USD" || decoded.AsOf.IsZero() || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing native period, currency or cache protection")
	}
	for i, expected := range []float64{27, 35, 0, 9} {
		item := decoded.Data[i]
		if item.TeamID != ids[i] || item.Status != 200 || item.UsedUSD == nil || *item.UsedUSD != expected {
			t.Fatalf("incorrect Team attribution: %+v", item)
		}
	}
	a, b := decoded.Data[1], decoded.Data[0]
	if *a.MonthlyLimitUSD != 80 || *a.RemainingUSD != 45 || len(a.Budgets) != 3 || *b.MonthlyLimitUSD != 10 || *b.RemainingUSD != 0 {
		t.Fatalf("budgets must not be added together or use cached spend: %+v %+v", a, b)
	}
	for _, item := range decoded.Data[2:4] {
		if item.MonthlyLimitUSD != nil || item.RemainingUSD != nil {
			t.Fatal("absent, zero or warning budget was treated as an enforced cap")
		}
	}
	missing := decoded.Data[4]
	if missing.Status != 404 || missing.UsedUSD != nil || len(missing.Budgets) != 0 {
		t.Fatal("missing Team was reported as zero consumption")
	}
	for _, budget := range a.Budgets {
		if budget.UsedUSD != 35 || budget.RemainingUSD != max(0, budget.AmountUSD-35) {
			t.Fatal("per-budget remaining is incorrect")
		}
	}
}
