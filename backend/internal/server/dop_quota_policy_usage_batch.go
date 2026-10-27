package server

import (
	"context"
	"database/sql"
	"strings"

	"gorm.io/gorm"
)

type dopQuotaPolicyUsageTarget struct{ Scope, ScopeID string }
type dopQuotaPolicyUsageResult struct {
	Usage     QuotaPolicyUsage
	Supported bool
}

func readDOPQuotaPolicyUsages(ctx context.Context, store Store, targets []dopQuotaPolicyUsageTarget) ([]dopQuotaPolicyUsageResult, error) {
	// Concrete type preserves overrides on Store wrappers.
	if db, ok := store.(*GormStore); ok {
		return db.readDOPQuotaPolicyUsages(ctx, targets)
	}
	result := make([]dopQuotaPolicyUsageResult, len(targets))
	for i, target := range targets {
		usage, supported, err := store.GetQuotaPolicyUsage(target.Scope, target.ScopeID)
		if err != nil {
			return nil, err
		}
		result[i] = dopQuotaPolicyUsageResult{usage, supported}
	}
	return result, nil
}

func (s *GormStore) readDOPQuotaPolicyUsages(ctx context.Context, targets []dopQuotaPolicyUsageTarget) ([]dopQuotaPolicyUsageResult, error) {
	result := make([]dopQuotaPolicyUsageResult, len(targets))
	users, keys := map[string]bool{}, map[string]bool{}
	for i, target := range targets {
		scope, id := strings.ToLower(strings.TrimSpace(target.Scope)), strings.TrimSpace(target.ScopeID)
		if id == "" {
			continue
		}
		switch scope {
		case "user":
			if id == allUsersQuotaScopeID {
				continue
			}
			users[id] = true
		case "key", "api_key":
			keys[id] = true
		default:
			continue
		}
		result[i].Supported = true
	}
	if len(users)+len(keys) == 0 {
		return result, nil
	}
	usageUser, usageKey := map[string]QuotaPolicyUsage{}, map[string]QuotaPolicyUsage{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := s.databaseNow(tx)
		if err != nil {
			return err
		}
		periods, err := currentQuotaPeriods(tx, now)
		if err != nil {
			return err
		}
		periodQuery := "((scope = ? AND bucket = ?) OR (scope = ? AND bucket = ?))"
		periodArgs := []any{"day", periods.Day, "month", periods.Month}
		userIDs, bucketIDs, keyIDs := []string{}, []string{}, []string{}
		for id := range users {
			userIDs = append(userIDs, id)
			bucketIDs = append(bucketIDs, userQuotaBucketKey(id))
		}
		for id := range keys {
			keyIDs = append(keyIDs, id)
		}
		for _, group := range []struct {
			ids  []string
			user bool
		}{{bucketIDs, true}, {keyIDs, false}} {
			for start := 0; start < len(group.ids); start += 400 {
				end := start + 400
				if end > len(group.ids) {
					end = len(group.ids)
				}
				var buckets []QuotaBucket
				query := tx.Where("key_id IN ?", group.ids[start:end]).Where(periodQuery, periodArgs...)
				if group.user {
					query = query.Where("attributed_user_id = '' OR key_id = 'user:' || attributed_user_id")
				} else {
					query = query.Where("attributed_user_id = ?", unattributedQuotaUserID)
				}
				if err := query.Order("key_id, scope, bucket, attributed_user_id").Find(&buckets).Error; err != nil {
					return err
				}
				seen := map[string]bool{}
				for _, b := range buckets {
					id := b.KeyID
					if group.user {
						id = strings.TrimPrefix(id, "user:")
						if b.AttributedUserID != id && b.AttributedUserID != "" {
							continue
						}
					} else if b.AttributedUserID != unattributedQuotaUserID {
						continue
					}
					pair := id + "\x00" + b.Scope
					if seen[pair] {
						continue
					}
					seen[pair] = true
					if group.user {
						mergeDOPBatchCounter(usageUser, id, b.Scope, b.QuotaCounter)
					} else {
						mergeDOPBatchCounter(usageKey, id, b.Scope, b.QuotaCounter)
					}
				}
			}
		}
		for start := 0; start < len(userIDs); start += 400 {
			end := start + 400
			if end > len(userIDs) {
				end = len(userIDs)
			}
			var rows []struct {
				AttributedUserID, Scope string
				QuotaCounter
			}
			if err := tx.Table("quota_buckets").Select("attributed_user_id, scope, SUM(requests) AS requests, SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens, SUM(total_tokens) AS total_tokens, SUM(cost_usd) AS cost_usd").Where("attributed_user_id IN ? AND key_id NOT LIKE ?", userIDs[start:end], "user:%").Where(periodQuery, periodArgs...).Group("attributed_user_id, scope").Scan(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				mergeDOPBatchCounter(usageUser, row.AttributedUserID, row.Scope, row.QuotaCounter)
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	for i, target := range targets {
		if result[i].Supported {
			id := strings.TrimSpace(target.ScopeID)
			if strings.EqualFold(strings.TrimSpace(target.Scope), "user") {
				result[i].Usage = usageUser[id]
			} else {
				result[i].Usage = usageKey[id]
			}
		}
	}
	return result, nil
}
