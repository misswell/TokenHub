package server

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
)

type dopTeamBudgetUsage struct {
	BudgetID     string  `json:"budget_id"`
	AmountUSD    float64 `json:"amount_usd"`
	UsedUSD      float64 `json:"used_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	Enforcement  string  `json:"enforcement"`
	Enforced     bool    `json:"enforced"`
}

type dopBatchTeamUsage struct {
	TeamID          string               `json:"team_id"`
	Status          int                  `json:"status"`
	TeamStatus      string               `json:"team_status,omitempty"`
	UsedUSD         *float64             `json:"used_usd,omitempty"`
	MonthlyLimitUSD *float64             `json:"monthly_limit_usd,omitempty"`
	RemainingUSD    *float64             `json:"remaining_usd,omitempty"`
	Budgets         []dopTeamBudgetUsage `json:"budgets"`
}

func (api *dopBatchAPI) serveTeamUsage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		TeamIDs []string `json:"team_ids"`
	}
	if err := api.server.decodeJSON(w, r, &request); err != nil {
		writeError(w, r, err)
		return
	}
	if len(request.TeamIDs) == 0 || len(request.TeamIDs) > dopBatchLimit {
		writeError(w, r, NewHTTPError(400, "invalid_batch_size", "team_ids must contain 1 to 20000 entries"))
		return
	}
	seen := make(map[string]bool, len(request.TeamIDs))
	for _, id := range request.TeamIDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			writeError(w, r, NewHTTPError(400, "invalid_team_ids", "team IDs must be nonempty, unique and at most 256 characters"))
			return
		}
		seen[id] = true
	}
	data, at, err := api.store.readDOPBatchTeamUsage(r.Context(), request.TeamIDs)
	if err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	status := http.StatusOK
	for _, item := range data {
		if item.Status != http.StatusOK {
			status = http.StatusMultiStatus
		}
	}
	writeJSON(w, status, map[string]any{
		"data": data, "period": at.Format("2006-01"), "timezone": "UTC", "as_of": at, "currency": "USD",
	})
}

// Match native Team budget enforcement: UTC month, current project membership,
// and recorded spend even for disabled projects or deleted/reassigned Keys.
func (s *GormStore) readDOPBatchTeamUsage(ctx context.Context, ids []string) ([]dopBatchTeamUsage, time.Time, error) {
	var results []dopBatchTeamUsage
	var at time.Time
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		at, err = s.databaseNow(tx)
		if err != nil {
			return err
		}
		at = at.UTC()
		var teams []AdminResource
		if err := tx.Select("id", "status").Where("kind = ? AND id IN ?", "teams", ids).Find(&teams).Error; err != nil {
			return err
		}
		existing := make(map[string]string, len(teams))
		for _, team := range teams {
			existing[team.ID] = team.Status
		}
		budgets, err := readDOPTeamBudgets(tx, ids, at)
		if err != nil {
			return err
		}
		var totals []struct {
			TeamID  string
			UsedUSD float64
		}
		period := at.Format("2006-01")
		if err := tx.Table("usage_records AS u").
			Select("p.team_id AS team_id, COALESCE(SUM(u.cost_usd), 0) AS used_usd").
			Joins("JOIN projects AS p ON p.id = u.project_id").
			Where("p.team_id IN ? AND u.created_at >= ? AND u.created_at < ?", ids, periodStart(period), periodEnd(period)).
			Group("p.team_id").Scan(&totals).Error; err != nil {
			return err
		}
		used := make(map[string]float64, len(totals))
		for _, total := range totals {
			used[total.TeamID] = total.UsedUSD
		}
		results = make([]dopBatchTeamUsage, 0, len(ids))
		for _, id := range ids {
			item := dopBatchTeamUsage{TeamID: id, Status: http.StatusNotFound, Budgets: []dopTeamBudgetUsage{}}
			if teamStatus, ok := existing[id]; ok {
				cost := used[id]
				item.Status, item.TeamStatus, item.UsedUSD = http.StatusOK, teamStatus, &cost
				for _, budget := range budgets[id] {
					budget.UsedUSD, budget.RemainingUSD = cost, math.Max(0, budget.AmountUSD-cost)
					item.Budgets = append(item.Budgets, budget)
					if budget.Enforced && (item.MonthlyLimitUSD == nil || budget.AmountUSD < *item.MonthlyLimitUSD) {
						amount := budget.AmountUSD
						item.MonthlyLimitUSD = &amount
					}
				}
				if item.MonthlyLimitUSD != nil {
					remaining := math.Max(0, *item.MonthlyLimitUSD-cost)
					item.RemainingUSD = &remaining
				}
			}
			results = append(results, item)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return results, at, err
}
