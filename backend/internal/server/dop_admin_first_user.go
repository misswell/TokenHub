package server

import (
	"context"
	"net/http"
	"strings"
)

// Only use the optimized path for the concrete store: wrappers may override
// ListAdminUsers and must retain their own identity semantics.
func firstDOPAdminUser(ctx context.Context, store Store) (AdminUser, bool, error) {
	if db, ok := store.(*GormStore); ok {
		var user AdminUser
		result := db.criticalReadDatabase().WithContext(ctx).Order("created_at asc").Limit(1).Find(&user)
		return publicAdminUser(user), result.RowsAffected > 0, result.Error
	}
	users := store.ListAdminUsers()
	if len(users) == 0 {
		return AdminUser{}, false, nil
	}
	return users[0], true, nil
}

func (api *dopBatchAPI) authorizeDOPAdminUser(w http.ResponseWriter, r *http.Request) (AdminUser, bool) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, r, NewHTTPError(401, "invalid_admin_token", "Invalid admin token"))
		return AdminUser{}, false
	}
	if token == strings.TrimSpace(api.server.config.AdminToken) {
		user, found, err := firstDOPAdminUser(r.Context(), api.server.store)
		if err != nil {
			writeError(w, r, NewHTTPError(500, "admin_user_query_failed", "Failed to query admin user"))
			return AdminUser{}, false
		}
		if found {
			return user, true
		}
		return AdminUser{ID: "dev_admin", Username: "dev_admin", Name: "开发管理员", Email: "admin@tokenhub.local", Role: "admin", Status: StatusActive}, true
	}
	user, ok := api.server.store.ValidateAdminSession(token)
	if !ok {
		writeError(w, r, NewHTTPError(401, "invalid_admin_token", "Invalid admin token"))
		return AdminUser{}, false
	}
	return user, true
}
func (api *dopBatchAPI) requireDOPAdmin(w http.ResponseWriter, r *http.Request, resource string) (AdminUser, bool) {
	user, ok := api.authorizeDOPAdminUser(w, r)
	if !ok {
		return AdminUser{}, false
	}
	if !canAdmin(user.Role, resource, r.Method) {
		writeError(w, r, NewHTTPError(403, "admin_forbidden", "Admin role is not allowed to perform this action"))
		return AdminUser{}, false
	}
	return user, true
}
func (api *dopBatchAPI) serveDOPUserDelete(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/api/admin/users/"
	if r.Method != http.MethodDelete || r.Header.Get("X-DOP-Operation") == "true" || !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" || id == "import" || strings.Contains(id, "/") {
		return false
	}
	actor, ok := api.requireDOPAdmin(w, r, "identity")
	if ok {
		api.server.serveAdminUserDelete(w, r, actor, id)
	}
	return true
}
