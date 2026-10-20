//go:build integration

package server

import (
	"context"
	"testing"
)

func TestDOPBatchTeamUsagePostgres(t *testing.T) {
	store, _, _ := openSharedPostgresStores(t)
	teamID := NewID("dop_team_usage")
	budgetID := "budget-" + teamID
	var projectID, usageID string
	t.Cleanup(func() {
		if usageID != "" {
			if err := store.db.Where("id = ?", usageID).Delete(&UsageRecord{}).Error; err != nil {
				t.Errorf("delete PostgreSQL Team usage fixture: %v", err)
			}
		}
		if projectID != "" {
			if err := store.db.Where("id = ?", projectID).Delete(&Project{}).Error; err != nil {
				t.Errorf("delete PostgreSQL Team project fixture: %v", err)
			}
		}
		if err := store.db.Where("kind = ? AND id = ?", "budgets", budgetID).Delete(&AdminResource{}).Error; err != nil {
			t.Errorf("delete PostgreSQL Team budget fixture: %v", err)
		}
		if err := store.db.Where("kind = ? AND id = ?", "teams", teamID).Delete(&AdminResource{}).Error; err != nil {
			t.Errorf("delete PostgreSQL Team fixture: %v", err)
		}
	})
	at, err := store.databaseNow(store.db)
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.CreateResourceChecked("teams", AdminResource{ID: teamID, Name: teamID})
	if err != nil {
		t.Fatal(err)
	}
	project := store.CreateProject(Project{Name: teamID, TeamID: team.ID, Status: StatusActive})
	if project.ID == "" {
		t.Fatal("failed to create PostgreSQL test project")
	}
	projectID = project.ID
	dopTestTeamBudget(t, store, budgetID, team.ID, "", 100, "hard", StatusActive)
	usageID = NewID("usage")
	if err := store.db.Create(&UsageRecord{
		ID: usageID, RequestID: NewID("req"), ProjectID: project.ID,
		ModelName: "gpt-4.1-mini", CostUSD: 20, CreatedAt: at,
	}).Error; err != nil {
		t.Fatal(err)
	}
	items, _, err := store.readDOPBatchTeamUsage(context.Background(), []string{team.ID})
	if err != nil || len(items) != 1 {
		t.Fatalf("PostgreSQL Team aggregation failed: %v", err)
	}
	item := items[0]
	if item.UsedUSD == nil || *item.UsedUSD != 20 || item.MonthlyLimitUSD == nil || *item.MonthlyLimitUSD != 100 || item.RemainingUSD == nil || *item.RemainingUSD != 80 {
		t.Fatalf("incorrect PostgreSQL Team budget result: %+v", item)
	}
}
