package server

import (
	"encoding/json"
	"net/http"
)

// DOP catalog reads avoid native per-policy counter scans. Only platform-admin,
// explicitly opted-in callers receive immutable identity proofs for matching.
func (api *dopBatchAPI) serveDOPUsersInfo(w http.ResponseWriter, r *http.Request) {
	var users []AdminUser
	if err := api.store.db.Find(&users).Error; err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	var bindings []dopOAuthIdentity
	if err := api.store.db.Find(&bindings).Error; err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	byUser := make(map[string]dopOAuthIdentity, len(bindings))
	for _, binding := range bindings {
		byUser[binding.UserID] = binding
	}
	rows := make([]map[string]any, 0, len(users))
	for _, user := range users {
		bytes, err := json.Marshal(publicAdminUser(user))
		if err != nil {
			writeError(w, r, err)
			return
		}
		var value map[string]any
		if err = json.Unmarshal(bytes, &value); err != nil {
			writeError(w, r, err)
			return
		}
		if binding, ok := byUser[user.ID]; ok && binding.ProviderID != dopNoLoginProvider {
			value["dop_identity"] = map[string]any{"provider_id": binding.ProviderID, "subject": binding.Subject}
		}
		rows = append(rows, value)
	}
	writeJSON(w, 200, map[string]any{"data": rows})
}
func (api *dopBatchAPI) serveDOPOwnerKeys(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("dop_owner_id")
	if owner == "" || len(owner) > 256 {
		writeError(w, r, NewHTTPError(400, "invalid_owner", "Invalid owner"))
		return
	}
	var keys []APIKey
	if err := api.store.db.Where("owner_user_id = ?", owner).Find(&keys).Error; err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	writeJSON(w, 200, map[string]any{"data": keys})
}

func (api *dopBatchAPI) serveDOPSingleTeamUsage(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || len(id) > 256 {
		writeError(w, r, NewHTTPError(400, "invalid_team_id", "Invalid Team ID"))
		return
	}
	rows, at, err := api.store.readDOPBatchTeamUsage(r.Context(), []string{id})
	if err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	if rows[0].Status != 200 {
		writeError(w, r, NewHTTPError(404, "team_not_found", "Team not found"))
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows[0].Budgets, "used_usd": rows[0].UsedUSD, "period": at.Format("2006-01"), "timezone": "UTC", "as_of": at, "currency": "USD"})
}
