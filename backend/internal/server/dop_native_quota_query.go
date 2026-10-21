package server

import "net/http"

// Only the DOP entrypoint intercepts exact-ID reads, including existing callers
// without the DOP header. Other requests retain their original routing.
func (api *dopBatchAPI) serveDOPQuotaPolicyIDQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/api/admin/resources/quota-policies" ||
		r.URL.Query().Get("id") == "" || r.Header.Get("X-DOP-Operation") == "true" {
		return false
	}
	user, ok := api.server.requireAdmin(w, r, adminResourcePermission(r.URL.Path), r.Method)
	if !ok {
		return true
	}
	items, err := api.dopQuotaPolicyQuery(r)
	if err != nil {
		writeError(w, r, err)
		return true
	}
	items = api.server.filterResourcesForUser(user, "quota-policies", items)
	items = api.server.attachQuotaPolicyUsage(items)
	writeJSON(w, http.StatusOK, map[string]any{"data": redactAdminResourcesForResponse("quota-policies", items)})
	return true
}

func (api *dopBatchAPI) dopQuotaPolicyQuery(r *http.Request) ([]AdminResource, error) {
	id := r.URL.Query().Get("id")
	if id == "" {
		return api.store.ListResources("quota-policies"), nil
	}
	items := make([]AdminResource, 0, 1)
	err := api.store.db.WithContext(r.Context()).Where("kind = ? AND id = ?", "quota-policies", id).Find(&items).Error
	return items, err
}
