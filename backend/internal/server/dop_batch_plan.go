package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type dopBatchPlan struct {
	groups [][]int
	keys   [][]string
}

func dopBatchIdentityKeys(req dopProvisionRequest) []string {
	username, email := strings.ToLower(strings.TrimSpace(req.Username)), strings.ToLower(strings.TrimSpace(req.Email))
	if username == "" {
		username = email
	}
	var keys []string
	for _, identity := range [][]string{{"username", username}, {"email", email}, {"oauth", req.OAuthProviderID, req.OAuthSubject}} {
		if identity[1] == "" {
			continue
		}
		encoded, _ := json.Marshal(identity)
		keys = append(keys, "identity:"+dopBatchHash(encoded))
	}
	return keys
}

func (api *dopBatchAPI) planConcurrent(ctx context.Context, operation string, items []dopBatchItem) (dopBatchPlan, error) {
	ids := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		if item.ID != "" && !seen[item.ID] {
			ids = append(ids, item.ID)
			seen[item.ID] = true
		}
	}
	resources := make(map[string]AdminResource)
	projects := make(map[string]Project)
	if len(ids) > 0 {
		switch operation {
		case "user-quotas", "team-budgets":
			kind := "quota-policies"
			if operation == "team-budgets" {
				kind = "budgets"
			}
			var rows []AdminResource
			if err := api.store.db.WithContext(ctx).Select("id", "fields").Where("kind = ? AND id IN ?", kind, ids).Find(&rows).Error; err != nil {
				return dopBatchPlan{}, err
			}
			for _, row := range rows {
				resources[row.ID] = row
			}
		case "projects/status":
			var rows []Project
			if err := api.store.db.WithContext(ctx).Select("id", "team_id").Where("id IN ?", ids).Find(&rows).Error; err != nil {
				return dopBatchPlan{}, err
			}
			for _, row := range rows {
				projects[row.ID] = row
			}
		}
	}
	keys := make([][]string, len(items))
	for i, item := range items {
		var body struct {
			OwnerUserID string         `json:"owner_user_id"`
			Fields      map[string]any `json:"fields"`
		}
		_ = json.Unmarshal(item.Body, &body)
		switch operation {
		case "users/provision":
			var req dopProvisionRequest
			_ = json.Unmarshal(item.Body, &req)
			keys[i] = dopBatchIdentityKeys(req)
			if api.executor.options.DisableParallelProvision && req.TeamID != "" {
				keys[i] = append(keys[i], "team:"+req.TeamID)
			}
		case "user-quotas", "team-budgets":
			prefix := "user:"
			if operation == "team-budgets" {
				prefix = "team:"
			}
			scope := stringField(body.Fields, "scope_id")
			if item.ID != "" {
				keys[i] = append(keys[i], operation+":"+item.ID)
				if row, ok := resources[item.ID]; ok {
					scope = stringField(row.Fields, "scope_id")
				}
			}
			if scope != "" {
				keys[i] = append(keys[i], prefix+scope)
			}
		case "projects/status":
			if item.ID != "" {
				keys[i] = append(keys[i], "project:"+item.ID)
			}
			if project, ok := projects[item.ID]; ok && project.TeamID != "" {
				keys[i] = append(keys[i], "team:"+project.TeamID)
			}
		case "users/status":
			if item.ID != "" {
				keys[i] = append(keys[i], "user:"+item.ID)
			}
		case "api-keys/status":
			if item.ID != "" {
				keys[i] = append(keys[i], "api_key:"+item.ID)
			}
		case "api-keys/create":
			if body.OwnerUserID != "" {
				keys[i] = append(keys[i], "user:"+body.OwnerUserID)
			}
		}
		if len(keys[i]) == 0 {
			keys[i] = []string{fmt.Sprintf("item:%d:%s", i, item.ClientRef)}
		}
	}
	return dopBatchPlan{groups: groupDOPBatchDependencies(keys), keys: keys}, nil
}

// Connected components preserve input ordering even when dependencies alias.
func groupDOPBatchDependencies(keys [][]string) [][]int {
	parents := make([]int, len(keys))
	for i := range parents {
		parents[i] = i
	}
	find := func(i int) int {
		for parents[i] != i {
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	owners := make(map[string]int)
	for i, row := range keys {
		for _, key := range row {
			if owner, ok := owners[key]; ok {
				a, b := find(i), find(owner)
				parents[max(a, b)] = min(a, b)
			} else {
				owners[key] = i
			}
		}
	}
	groups := make(map[int][]int)
	for i := range keys {
		root := find(i)
		groups[root] = append(groups[root], i)
	}
	roots := make([]int, 0, len(groups))
	for root := range groups {
		roots = append(roots, root)
	}
	sort.Ints(roots)
	out := make([][]int, 0, len(roots))
	for _, root := range roots {
		out = append(out, groups[root])
	}
	return out
}
