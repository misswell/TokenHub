package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
)

// Existing IDs must be supplied on updates. New resources get server-generated
// IDs and are protected by the item receipt, rather than native POST upserts.
func (s *Server) serveDOPBatchResource(w http.ResponseWriter, r *http.Request, user AdminUser, operation string, item dopBatchItem) {
	kind, scope, amountField := "quota-policies", "user", "monthly_cost_usd"
	if operation == "team-budgets" {
		kind, scope, amountField = "budgets", "team", "amount_usd"
	}
	if item.Method != http.MethodPost && item.Method != http.MethodPatch {
		writeError(w, r, NewHTTPError(400, "invalid_item_method", "method must be POST or PATCH"))
		return
	}
	var patch AdminResource
	if err := json.Unmarshal(item.Body, &patch); err != nil || patch.ID != "" {
		writeError(w, r, NewHTTPError(400, "invalid_resource_body", "body must be a resource without an id"))
		return
	}
	if (item.Method == http.MethodPatch) != (item.ID != "") {
		writeError(w, r, NewHTTPError(400, "invalid_resource_id", "PATCH requires id; POST must omit id"))
		return
	}
	// Unlike generic PATCH, this amount API preserves unrelated fields. Scope
	// cannot be changed by an amount update, and status defaults are retained.
	if item.Method == http.MethodPatch {
		existing, err := s.findResource(kind, item.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if firstStringField(existing.Fields, "scope", "scope_type") != scope {
			writeError(w, r, NewHTTPError(400, "invalid_batch_scope", "Resource has a different scope"))
			return
		}
		if value := stringField(patch.Fields, "scope_id"); value != "" && value != stringField(existing.Fields, "scope_id") {
			writeError(w, r, NewHTTPError(400, "invalid_batch_scope", "An amount update cannot transfer a resource"))
			return
		}
		fields := make(map[string]any, len(existing.Fields)+len(patch.Fields))
		for key, value := range existing.Fields {
			fields[key] = value
		}
		for key, value := range patch.Fields {
			fields[key] = value
		}
		patch.Fields, patch.Description = fields, existing.Description
	}
	amount, exists := patch.Fields[amountField].(float64)
	if firstStringField(patch.Fields, "scope", "scope_type") != scope || strings.TrimSpace(stringField(patch.Fields, "scope_id")) == "" || !exists || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		writeError(w, r, NewHTTPError(400, "invalid_batch_amount", "A matching scope, scope_id and positive USD amount are required; use status operations to deny access"))
		return
	}
	if kind == "budgets" {
		if _, err := s.findResource("teams", stringField(patch.Fields, "scope_id")); err != nil {
			writeError(w, r, err)
			return
		}
		if patch.Fields["enforcement"] != "hard" {
			writeError(w, r, NewHTTPError(400, "invalid_budget_enforcement", "Team allocation budgets require enforcement=hard"))
			return
		}
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		writeError(w, r, err)
		return
	}
	r.Method = item.Method
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	if item.Method == http.MethodPost {
		s.serveAdminResourceCollectionPost(w, r, user, kind)
	} else {
		s.serveAdminResourcePatch(w, r, user, kind, item.ID)
	}
}
