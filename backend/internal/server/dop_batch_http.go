package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const dopBatchLimit = 20000

// DOPBatchHandler opts into the additive DOP API. The original Handler and its
// routes remain untouched. Call once at startup, before accepting requests.
func (s *Server) DOPBatchHandler() (http.Handler, error) {
	return s.DOPBatchHandlerWithOptions(DOPBatchOptions{})
}

// DOPBatchHandlerWithOptions configures opt-in concurrency. Omitted request
// concurrency always retains the original synchronous item loop.
func (s *Server) DOPBatchHandlerWithOptions(options DOPBatchOptions) (http.Handler, error) {
	store, ok := s.store.(*GormStore)
	if !ok {
		return nil, fmt.Errorf("DOP batch API requires a GormStore")
	}
	if err := store.db.AutoMigrate(&dopBatchReceipt{}, &dopBatchStatusMark{}, &dopOAuthIdentity{}); err != nil {
		return nil, fmt.Errorf("initialize DOP batch tables: %w", err)
	}
	if err := initializeDOPBatchIndex(store.db); err != nil {
		return nil, fmt.Errorf("initialize DOP quota lookup index: %w", err)
	}
	executor, err := newDOPBatchExecutor(store, options)
	if err != nil {
		return nil, err
	}
	api := &dopBatchAPI{server: s, store: store, executor: executor}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/dop/batch/", api.serveHTTP)
	api.registerIdentityRoutes(mux)
	mux.Handle("/", s.mux)
	return &dopBatchHTTPHandler{Handler: s.withPluginRuntimeSnapshot(s.cors(api.nativeSingleHandler(mux))), executor: executor}, nil
}

type dopBatchAPI struct {
	server   *Server
	store    *GormStore
	executor *dopBatchExecutor
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
	if operation == "teams/usage" {
		api.serveTeamUsage(w, r)
		return
	}
	switch operation {
	case "user-quotas", "team-budgets", "projects/status", "api-keys/status", "api-keys/create", "users/provision", "users/status":
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
		Items       []dopBatchItem `json:"items"`
		Concurrency *int           `json:"concurrency,omitempty"`
	}
	if err := api.server.decodeJSON(w, r, &request); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateDOPBatchItems(request.Items); err != nil {
		writeError(w, r, err)
		return
	}
	workers, err := api.executor.workers(request.Concurrency)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if workers > 1 {
		api.serveConcurrent(w, r, user, operation, key, request.Items, workers)
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
	for _, item := range request.Items {
		if r.Context().Err() != nil {
			return // Committed items are recoverable with the same key and refs.
		}
		results = append(results, api.executeItem(r, user, operation, key, item))
	}
	var execution map[string]any
	if request.Concurrency != nil {
		execution = map[string]any{"requested_concurrency": *request.Concurrency, "concurrency": 1, "mode": "serial"}
	}
	status, envelope := dopBatchSummary(results, execution)
	writeJSON(w, status, envelope)
}

// dopBatchSummary counts result states and builds the standard batch response
// envelope shared by the serial and concurrent paths. execution is included
// only when non-nil.
func dopBatchSummary(results []dopBatchResult, execution map[string]any) (int, map[string]any) {
	succeeded, pending, failed := 0, 0, 0
	for i := range results {
		switch results[i].State {
		case "succeeded":
			succeeded++
		case "pending":
			pending++
		default:
			failed++
		}
	}
	status := http.StatusOK
	if failed+pending > 0 {
		status = http.StatusMultiStatus
	}
	envelope := map[string]any{
		"data": results, "total": len(results), "succeeded": succeeded,
		"pending": pending, "failed": failed,
	}
	if execution != nil {
		envelope["execution"] = execution
	}
	return status, envelope
}

func validateDOPBatchItems(items []dopBatchItem) error {
	if len(items) == 0 || len(items) > dopBatchLimit {
		return NewHTTPError(400, "invalid_batch_size", "items must contain 1 to 20000 entries")
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
	if operation == "users/status" {
		s.serveDOPManagedUserStatus(w, r, user, item)
		return
	}
	if strings.HasPrefix(operation, "native/") {
		api.dispatchDOPNative(s, w, r, user, operation, item)
		return
	}
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
