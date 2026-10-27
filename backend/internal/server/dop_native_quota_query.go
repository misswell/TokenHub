package server

import "net/http"

// The DOP entrypoint enriches ordinary quota reads, including exact-ID reads.
// Explicit DOP catalog requests continue through the existing fast path.
func (api *dopBatchAPI) serveDOPQuotaPolicyQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/api/admin/resources/quota-policies" ||
		r.Header.Get("X-DOP-Operation") == "true" {
		return false
	}
	user, ok := api.requireDOPAdmin(w, r, adminResourcePermission(r.URL.Path))
	if !ok {
		return true
	}
	items, err := api.dopQuotaPolicyQuery(r)
	if err != nil {
		writeError(w, r, err)
		return true
	}
	items = api.server.filterResourcesForUser(user, "quota-policies", items)
	targets := make([]dopQuotaPolicyUsageTarget, len(items))
	for i, item := range items {
		targets[i] = dopQuotaPolicyUsageTarget{firstStringField(item.Fields, "scope", "scope_type"), firstStringField(item.Fields, "scope_id")}
	}
	results, err := readDOPQuotaPolicyUsages(r.Context(), api.server.store, targets)
	if err != nil {
		writeError(w, r, NewHTTPError(http.StatusInternalServerError, "quota_policy_usage_query_failed", "Failed to query quota policy usage"))
		return true
	}
	for i, result := range results {
		if result.Supported {
			usage := result.Usage
			items[i].CurrentUsage = &usage
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": redactAdminResourcesForResponse("quota-policies", items)})
	return true
}

func (api *dopBatchAPI) dopQuotaPolicyQuery(r *http.Request) ([]AdminResource, error) {
	id := r.URL.Query().Get("id")
	if id == "" {
		if _, ok := api.server.store.(*GormStore); !ok && r.Header.Get("X-DOP-Operation") != "true" {
			return api.server.store.ListResources("quota-policies"), nil
		}
		return api.store.ListResourcesContext(r.Context(), "quota-policies")
	}
	items := make([]AdminResource, 0, 1)
	err := api.store.db.WithContext(r.Context()).Where("kind = ? AND id = ?", "quota-policies", id).Find(&items).Error
	return items, err
}
