package server

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

func readDOPTeamBudgets(tx *gorm.DB, ids []string, at time.Time) (map[string][]dopTeamBudgetUsage, error) {
	// Only constant JSON field names enter SQL; all requested IDs are bound.
	scope := statementJSON(tx, "fields", "scope")
	scopeID := statementJSON(tx, "fields", "scope_id")
	legacyID := statementJSON(tx, "fields", "team_id")
	var resources []AdminResource
	if err := tx.Where("kind = ? AND status = ?", "budgets", StatusActive).
		Where("LOWER(TRIM("+scope+")) = ?", "team").
		Where("COALESCE(NULLIF(TRIM("+scopeID+"), ''), TRIM("+legacyID+")) IN ?", ids).
		Order("id").Find(&resources).Error; err != nil {
		return nil, err
	}
	budgets := make(map[string][]dopTeamBudgetUsage)
	period := at.Format("2006-01")
	for _, resource := range resources {
		ref := strings.TrimSpace(stringField(resource.Fields, "period_ref"))
		if ref != "" && normalizeBillingPeriod(ref, at) != period {
			continue
		}
		amount := float64Field(resource.Fields, "amount_usd")
		mode := strings.ToLower(strings.TrimSpace(stringField(resource.Fields, "enforcement")))
		if mode == "" {
			mode = "hard"
		}
		id := budgetScopeID(resource, "team")
		budgets[id] = append(budgets[id], dopTeamBudgetUsage{
			BudgetID: resource.ID, AmountUSD: amount, Enforcement: mode,
			Enforced: amount > 0 && budgetEnforced(resource),
		})
	}
	return budgets, nil
}
