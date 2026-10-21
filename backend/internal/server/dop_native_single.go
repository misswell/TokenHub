package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// The DOP entrypoint adds exact-ID quota reads. Durable single-item writes still
// require a platform administrator to opt in explicitly; the original handlers
// run in the receipt transaction without a one-item batch HTTP request.
func (api *dopBatchAPI) nativeSingleHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if api.serveDOPQuotaPolicyIDQuery(w, r) {
			return
		}
		capability := r.URL.Path == "/api/admin/dop/capabilities"
		if !capability && r.Header.Get("X-DOP-Operation") != "true" {
			next.ServeHTTP(w, r)
			return
		}
		operation, item, found := dopNativeSingleRoute(r)
		if !capability && !found {
			next.ServeHTTP(w, r)
			return
		}
		actor, ok := api.server.authorizeAdminUser(w, r)
		if !ok {
			return
		}
		if !isPlatformAdminRole(actor.Role) {
			writeError(w, r, NewHTTPError(403, "dop_single_forbidden", "A platform administrator is required"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if capability {
			if r.Method != http.MethodGet {
				jsonMethodNotAllowed(http.MethodGet)(w, r)
				return
			}
			writeJSON(w, 200, map[string]any{"native_single_receipts": true, "managed_single_users": true, "version": 1})
			return
		}
		if operation == "native/users-info" {
			api.serveDOPUsersInfo(w, r)
			return
		}
		if operation == "native/quotas-info" {
			items, err := api.dopQuotaPolicyQuery(r)
			if err != nil {
				writeError(w, r, err)
				return
			}
			writeJSON(w, 200, map[string]any{"data": items})
			return
		}
		if operation == "native/keys-info" {
			api.serveDOPOwnerKeys(w, r)
			return
		}
		if operation == "native/team-usage" {
			api.serveDOPSingleTeamUsage(w, r, item.ID)
			return
		}
		if operation == "native/user-usage" {
			api.serveDOPSingleUsage(w, r, item.ID)
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 128 {
			writeError(w, r, NewHTTPError(400, "invalid_idempotency_key", "Idempotency-Key is required"))
			return
		}
		var body map[string]json.RawMessage
		if err := api.server.decodeJSON(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		if body == nil {
			writeError(w, r, NewHTTPError(400, "invalid_resource_body", "An object is required"))
			return
		}
		if operation == "api-keys/create" {
			body["project_id"], _ = json.Marshal(item.ID)
			item.ID = ""
		}
		if operation == "native/users/create" || operation == "native/users/bind" {
			provider, subject := r.Header.Get("X-DOP-OAuth-Provider"), r.Header.Get("X-DOP-OAuth-Subject")
			decoded, err := base64.RawURLEncoding.DecodeString(subject)
			if err != nil || !utf8.Valid(decoded) || strings.TrimSpace(string(decoded)) == "" || len(decoded) == 0 || len(decoded) > 512 || len(provider) > 128 || provider == "" {
				writeError(w, r, NewHTTPError(400, "invalid_oauth_identity", "A trusted provider and exact base64url subject are required"))
				return
			}
			body["dop_provider"], _ = json.Marshal(provider)
			body["dop_subject"], _ = json.Marshal(string(decoded))
		}
		var err error
		item.Body, err = json.Marshal(body)
		if err != nil {
			writeError(w, r, err)
			return
		}
		result := api.executeItem(r, actor, operation, key, item)
		if result.Replayed {
			w.Header().Set("X-DOP-Replayed", "true")
		} else {
			w.Header().Set("X-DOP-Replayed", "false")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.Status)
		var encoded bytes.Buffer
		if err := json.Compact(&encoded, result.Body); err != nil {
			_, _ = w.Write(result.Body)
			return
		}
		_, _ = w.Write(encoded.Bytes())
	})
}

func dopNativeSingleRoute(r *http.Request) (string, dopBatchItem, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	item := dopBatchItem{ClientRef: "single", Method: r.Method}
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/resources/quota-policies" && r.URL.Query().Get("dop_user_id") != "" {
		item.ID = r.URL.Query().Get("dop_user_id")
		return "native/user-usage", item, true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/resources/budgets" && r.URL.Query().Get("dop_team_id") != "" {
		item.ID = r.URL.Query().Get("dop_team_id")
		return "native/team-usage", item, true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/users" {
		return "native/users-info", item, true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/resources/quota-policies" {
		return "native/quotas-info", item, true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/api-keys" && r.URL.Query().Get("dop_owner_id") != "" {
		return "native/keys-info", item, true
	}
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "admin" {
		return "", item, false
	}
	if len(parts) == 3 && parts[2] == "users" && r.Method == http.MethodPost {
		return "native/users/create", item, true
	}
	if len(parts) == 4 && parts[2] == "users" && r.Method == http.MethodPatch {
		item.ID = parts[3]
		if r.Header.Get("X-DOP-OAuth-Provider") != "" {
			return "native/users/bind", item, true
		}
		return "users/status", item, true
	}
	if len(parts) == 5 && parts[2] == "projects" && parts[4] == "keys" && r.Method == http.MethodPost {
		item.ID = parts[3]
		return "api-keys/create", item, true
	}
	if len(parts) == 4 && r.Method == http.MethodPatch {
		item.ID = parts[3]
		if parts[2] == "projects" {
			return "projects/status", item, true
		}
		if parts[2] == "api-keys" {
			return "api-keys/status", item, true
		}
	}
	if parts[2] == "projects" && len(parts) == 3 && r.Method == http.MethodPost {
		return "native/projects/create", item, true
	}
	if parts[2] == "resources" && (len(parts) == 4 || len(parts) == 5) {
		kind := parts[3]
		if kind != "teams" && kind != "project-members" && kind != "quota-policies" && kind != "budgets" {
			return "", item, false
		}
		if len(parts) == 4 && r.Method == http.MethodPost {
			return "native/resource/" + kind, item, true
		}
		if len(parts) == 5 && r.Method == http.MethodPatch {
			item.ID = parts[4]
			return "native/resource/" + kind, item, true
		}
	}
	return "", item, false
}

func (api *dopBatchAPI) serveDOPSingleUsage(w http.ResponseWriter, r *http.Request, id string) {
	if strings.TrimSpace(id) == "" || len(id) > 256 {
		writeError(w, r, NewHTTPError(400, "invalid_user_id", "Invalid User ID"))
		return
	}
	data, periods, at, err := api.store.readDOPBatchUsage(r.Context(), []dopBatchUsageInput{{UserID: id}})
	if err != nil {
		writeError(w, r, dopBatchSafeError(err))
		return
	}
	if data[0].Status != 200 {
		writeError(w, r, NewHTTPError(404, "user_not_found", "User not found"))
		return
	}
	// Native resource collection envelope plus filtered current-period metadata.
	writeJSON(w, 200, map[string]any{"data": []any{}, "current_usage": data[0].CurrentUsage, "dop_identity": data[0].Identity, "periods": periods, "as_of": at, "currency": "USD"})
}
