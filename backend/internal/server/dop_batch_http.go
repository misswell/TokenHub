package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const dopBatchLimit = 12000

// DOPBatchHandler opts into the additive DOP API. The original Handler and its
// routes remain untouched. Call once at startup, before accepting requests.
func (s *Server) DOPBatchHandler() (http.Handler, error) {
	store, ok := s.store.(*GormStore)
	if !ok {
		return nil, fmt.Errorf("DOP batch API requires a GormStore")
	}
	if err := store.db.AutoMigrate(&dopBatchReceipt{}, &dopBatchStatusMark{}, &dopOAuthIdentity{}); err != nil {
		return nil, fmt.Errorf("initialize DOP batch tables: %w", err)
	}
	api := &dopBatchAPI{server: s, store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/dop/batch/", api.serveHTTP)
	api.registerIdentityRoutes(mux)
	mux.Handle("/", s.mux)
	return s.withPluginRuntimeSnapshot(s.cors(mux)), nil
}

type dopBatchAPI struct {
	server *Server
	store  *GormStore
}

type dopBatchItem struct {
	ClientRef string          `json:"client_ref"`
	Method    string          `json:"method,omitempty"`
	ID        string          `json:"id,omitempty"`
	Body      json.RawMessage `json:"body"`
}

type dopBatchResult struct {
	ClientRef string          `json:"client_ref"`
	Status    int             `json:"status"`
	State     string          `json:"state"`
	Replayed  bool            `json:"replayed"`
	Body      json.RawMessage `json:"body"`
}

func (api *dopBatchAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// This service-to-service API deliberately requires a platform administrator.
	// Staff and leaders continue using the existing scoped single-item endpoints.
	user, ok := api.server.authorizeAdminUser(w, r)
	if !ok {
		return
	}
	if !isPlatformAdminRole(user.Role) {
		writeError(w, r, NewHTTPError(403, "dop_batch_forbidden", "DOP batch API requires a platform administrator"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		jsonMethodNotAllowed(http.MethodPost)(w, r)
		return
	}
	operation := strings.TrimPrefix(r.URL.Path, "/api/admin/dop/batch/")
	if operation == "users/usage" {
		api.serveUsage(w, r)
		return
	}
	switch operation {
	case "user-quotas", "team-budgets", "projects/status", "api-keys/status", "api-keys/create", "users/provision":
	default:
		writeError(w, r, NewHTTPError(404, "not_found", "Not found"))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, r, NewHTTPError(400, "invalid_idempotency_key", "Idempotency-Key must contain 1 to 128 characters"))
		return
	}
	var request struct {
		Items []dopBatchItem `json:"items"`
	}
	if err := api.server.decodeJSON(w, r, &request); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateDOPBatchItems(request.Items); err != nil {
		writeError(w, r, err)
		return
	}
	if operation == "users/provision" {
		var err error
		r, err = api.provisionPreflight(r)
		if err != nil {
			writeError(w, r, dopBatchSafeError(err))
			return
		}
	}
	results := make([]dopBatchResult, 0, len(request.Items))
	succeeded, pending, failed := 0, 0, 0
	for _, item := range request.Items {
		if r.Context().Err() != nil {
			return // Committed items are recoverable with the same key and refs.
		}
		result := api.executeItem(r, user, operation, key, item)
		switch result.State {
		case "succeeded":
			succeeded++
		case "pending":
			pending++
		default:
			failed++
		}
		results = append(results, result)
	}
	status := http.StatusOK
	if failed+pending > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{
		"data": results, "total": len(results), "succeeded": succeeded,
		"pending": pending, "failed": failed,
	})
}

func validateDOPBatchItems(items []dopBatchItem) error {
	if len(items) == 0 || len(items) > dopBatchLimit {
		return NewHTTPError(400, "invalid_batch_size", "items must contain 1 to 12000 entries")
	}
	refs := make(map[string]bool, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ClientRef) == "" || len(item.ClientRef) > 128 || refs[item.ClientRef] {
			return NewHTTPError(400, "invalid_client_ref", "client_ref must be unique and contain 1 to 128 characters")
		}
		refs[item.ClientRef] = true
		if len(item.Body) == 0 || bytes.Equal(bytes.TrimSpace(item.Body), []byte("null")) {
			return NewHTTPError(400, "invalid_batch_item", "Each item requires an object body")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item.Body, &object); err != nil || object == nil {
			return NewHTTPError(400, "invalid_batch_item", "Each item requires an object body")
		}
	}
	return nil
}

// An item executes on a transaction-scoped store using the original handlers.
// No loopback HTTP, token forwarding, or bypass of approval/audit behavior occurs.
func (api *dopBatchAPI) dispatch(s *Server, w http.ResponseWriter, r *http.Request, user AdminUser, operation string, item dopBatchItem) {
	if operation == "users/provision" {
		s.serveDOPProvision(w, r, user, item)
		return
	}
	if operation == "user-quotas" || operation == "team-budgets" {
		s.serveDOPBatchResource(w, r, user, operation, item)
		return
	}
	if operation == "projects/status" || operation == "api-keys/status" {
		s.serveDOPBatchStatus(w, r, user, operation, item)
		return
	}
	var body struct {
		ProjectID   string `json:"project_id"`
		OwnerUserID string `json:"owner_user_id"`
	}
	if err := json.Unmarshal(item.Body, &body); err != nil || body.ProjectID == "" || body.OwnerUserID == "" {
		writeError(w, r, NewHTTPError(400, "invalid_key_owner", "project_id and owner_user_id are required"))
		return
	}
	s.serveAdminProjectKeysPost(w, r, user, body.ProjectID)
}

func dopBatchErrorBody(err error) (int, json.RawMessage) {
	// Keep the native envelope while suppressing unexpected database details.
	w := &dopBatchResponse{header: make(http.Header)}
	writeError(w, &http.Request{Header: make(http.Header)}, dopBatchSafeError(err))
	return w.status, json.RawMessage(w.body.Bytes())
}

func dopBatchSafeError(err error) error {
	if AsHTTPError(err).Status >= http.StatusInternalServerError {
		return NewHTTPError(500, "batch_internal_error", "Batch operation could not be completed; retry with the same idempotency key")
	}
	return err
}

type dopBatchResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *dopBatchResponse) Header() http.Header { return w.header }
func (w *dopBatchResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *dopBatchResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
