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

type dopBatchUsageInput struct {
	UserID          string   `json:"user_id"`
	MonthlyLimitUSD *float64 `json:"monthly_limit_usd,omitempty"`
}

type dopUsageIdentityProof struct {
	ProviderID string `json:"provider_id"`
	Subject    string `json:"subject"`
	TeamID     string `json:"team_id"`
	TeamName   string `json:"team_name"`
	Role       string `json:"role"`
}

type dopBatchUserUsage struct {
	Identity        dopUsageIdentityProof `json:"dop_identity"`
	UserID          string                `json:"user_id"`
	Status          int                   `json:"status"`
	CurrentUsage    QuotaPolicyUsage      `json:"current_usage"`
	MonthlyLimitUSD *float64              `json:"monthly_limit_usd,omitempty"`
	RemainingUSD    *float64              `json:"remaining_usd,omitempty"`
}

func (api *dopBatchAPI) serveUsage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Users []dopBatchUsageInput `json:"users"`
	}
	if err := api.server.decodeJSON(w, r, &request); err != nil {
		writeError(w, r, err)
		return
	}
	if len(request.Users) == 0 || len(request.Users) > dopBatchLimit {
		writeError(w, r, NewHTTPError(400, "invalid_batch_size", "users must contain 1 to 20000 entries"))
		return
	}
	seen := make(map[string]bool, len(request.Users))
	for _, input := range request.Users {
		if strings.TrimSpace(input.UserID) == "" || len(input.UserID) > 256 || seen[input.UserID] {
			writeError(w, r, NewHTTPError(400, "invalid_user_ids", "user_id must be nonempty, unique and at most 256 characters"))
			return
		}
		seen[input.UserID] = true
		if limit := input.MonthlyLimitUSD; limit != nil && (*limit < 0 || math.IsNaN(*limit) || math.IsInf(*limit, 0)) {
			writeError(w, r, NewHTTPError(400, "invalid_monthly_limit", "monthly_limit_usd must be finite and nonnegative"))
			return
		}
	}
	data, periods, at, err := api.store.readDOPBatchUsage(r.Context(), request.Users)
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
	writeJSON(w, status, map[string]any{"data": data, "periods": periods, "as_of": at, "currency": "USD"})
}

// All users share one native quota timezone. Query count is constant with
// respect to user count; historical key attribution survives key reassignment.
func (s *GormStore) readDOPBatchUsage(ctx context.Context, inputs []dopBatchUsageInput) ([]dopBatchUserUsage, quotaPeriods, time.Time, error) {
	ids, bucketIDs := make([]string, 0, len(inputs)), make([]string, 0, len(inputs))
	for _, input := range inputs {
		ids = append(ids, input.UserID)
		bucketIDs = append(bucketIDs, userQuotaBucketKey(input.UserID))
	}
	var results []dopBatchUserUsage
	var periods quotaPeriods
	var at time.Time
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		at, err = s.databaseNow(tx)
		if err != nil {
			return err
		}
		periods, err = currentQuotaPeriods(tx, at)
		if err != nil {
			return err
		}
		var users []struct {
			ID, Role, TeamID, TeamName, ProviderID string
			OAuthSubject                           string `gorm:"column:oauth_subject"`
		}
		if err := tx.Table("admin_users AS u").Select("u.id,u.role,u.team_id,t.name AS team_name,i.provider_id,i.subject AS oauth_subject").
			Joins("LEFT JOIN dop_oauth_identities AS i ON i.user_id=u.id").
			Joins("LEFT JOIN admin_resources AS t ON t.id=u.team_id AND t.kind = ?", "teams").
			Where("u.id IN ?", ids).Scan(&users).Error; err != nil {
			return err
		}
		existing := make(map[string]dopUsageIdentityProof, len(users))
		for _, user := range users {
			if user.ProviderID == dopNoLoginProvider {
				user.ProviderID, user.OAuthSubject = "", ""
			}
			existing[user.ID] = dopUsageIdentityProof{ProviderID: user.ProviderID, Subject: user.OAuthSubject, TeamID: user.TeamID, TeamName: user.TeamName, Role: user.Role}
		}
		periodQuery := "((scope = ? AND bucket = ?) OR (scope = ? AND bucket = ?))"
		periodArgs := []any{"day", periods.Day, "month", periods.Month}
		var durable []QuotaBucket
		if err := tx.Where("key_id IN ?", bucketIDs).Where(periodQuery, periodArgs...).Find(&durable).Error; err != nil {
			return err
		}
		usage := make(map[string]QuotaPolicyUsage, len(inputs))
		for _, bucket := range durable {
			id := strings.TrimPrefix(bucket.KeyID, "user:")
			if bucket.AttributedUserID != id && bucket.AttributedUserID != "" {
				continue
			}
			mergeDOPBatchCounter(usage, id, bucket.Scope, bucket.QuotaCounter)
		}
		var attributed []struct {
			AttributedUserID string
			Scope            string
			QuotaCounter
		}
		if err := tx.Table("quota_buckets").
			Select("attributed_user_id, scope, SUM(requests) AS requests, SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens, SUM(total_tokens) AS total_tokens, SUM(cost_usd) AS cost_usd").
			Where("attributed_user_id IN ? AND key_id NOT LIKE ?", ids, "user:%").
			Where(periodQuery, periodArgs...).Group("attributed_user_id, scope").Scan(&attributed).Error; err != nil {
			return err
		}
		for _, row := range attributed {
			mergeDOPBatchCounter(usage, row.AttributedUserID, row.Scope, row.QuotaCounter)
		}
		results = make([]dopBatchUserUsage, 0, len(inputs))
		for _, input := range inputs {
			item := dopBatchUserUsage{UserID: input.UserID, Status: http.StatusOK, CurrentUsage: usage[input.UserID]}
			item.Identity = existing[input.UserID]
			if _, found := existing[input.UserID]; !found {
				item.Status = http.StatusNotFound
			} else if input.MonthlyLimitUSD != nil {
				remaining := math.Max(0, *input.MonthlyLimitUSD-item.CurrentUsage.Monthly.CostUSD)
				item.MonthlyLimitUSD, item.RemainingUSD = input.MonthlyLimitUSD, &remaining
			}
			results = append(results, item)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return results, periods, at, err
}

func mergeDOPBatchCounter(usage map[string]QuotaPolicyUsage, id, scope string, counter QuotaCounter) {
	value := usage[id]
	if scope == "day" {
		mergeQuotaCounterMax(&value.Daily, counter)
	} else {
		mergeQuotaCounterMax(&value.Monthly, counter)
	}
	usage[id] = value
}
