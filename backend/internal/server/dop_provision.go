package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type dopProvisionRequest struct {
	Authentication  string          `json:"authentication,omitempty"`
	Username        string          `json:"username"`
	Name            string          `json:"name"`
	Email           string          `json:"email"`
	TeamID          string          `json:"team_id"`
	ProjectID       string          `json:"project_id"`
	OAuthProviderID string          `json:"oauth_provider_id"`
	OAuthSubject    string          `json:"oauth_subject"`
	MonthlyLimitUSD float64         `json:"monthly_limit_usd"`
	Key             json.RawMessage `json:"key"`
}

// Each employee's identity, quota, Key, audits and encrypted receipt commit in
// one transaction. Native approvals are retained; no Key is issued ahead of an
// effective quota. There is no SMTP or bcrypt work in this creation path.
func (s *Server) serveDOPProvision(w http.ResponseWriter, r *http.Request, actor AdminUser, item dopBatchItem) {
	store := s.store.(*GormStore)
	req, keyBody, err := store.validateDOPProvision(s, r, item)
	if err != nil {
		writeError(w, r, err)
		return
	}
	user, err := store.createDOPProvisionUser(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	binding := dopOAuthIdentity{UserID: user.ID, ProviderID: req.OAuthProviderID, Subject: req.OAuthSubject, CreatedAt: time.Now().UTC()}
	if req.Authentication == "dop_only" {
		// Internal identity bookkeeping supports status/approval guards without
		// inventing an external OAuth subject or enabling an interactive login.
		binding.ProviderID, binding.Subject = dopNoLoginProvider, user.ID
	}
	if err := store.db.Create(&binding).Error; err != nil {
		writeError(w, r, writeConflict(err, "oauth_identity_conflict", "OAuth identity is already provisioned"))
		return
	}
	// Supply the exact new user to native validators instead of repeatedly
	// listing all employees for quota and Key ownership resolution.
	view := &dopProvisionStore{dopBatchLookupStore: &dopBatchLookupStore{GormStore: store, filterUsers: true, users: []AdminUser{user}}}
	native := &Server{store: view, config: s.config}
	native.recordAdminAudit(r, actor, "create", "admin_user", user.ID, "", user)
	if err := view.checkAudit(0); err != nil {
		writeError(w, r, err)
		return
	}
	quota := AdminResource{Name: "DOP " + user.ID, Status: StatusActive, Fields: map[string]any{
		"scope": "user", "scope_id": user.ID, "monthly_cost_usd": req.MonthlyLimitUSD,
	}}
	beforeAudit := view.auditCount
	quotaResult := dopProvisionCall(r, quota, func(response http.ResponseWriter, child *http.Request) {
		native.serveAdminResourceCollectionPost(response, child, actor, "quota-policies")
	})
	if quotaResult.status >= 400 {
		w.WriteHeader(quotaResult.status)
		_, _ = w.Write(quotaResult.body.Bytes())
		return
	}
	if err := view.checkAudit(beforeAudit); err != nil {
		writeError(w, r, err)
		return
	}
	result := map[string]any{"user": user, "authentication": req.Authentication, "quota": json.RawMessage(quotaResult.body.Bytes())}
	if quotaResult.status == http.StatusAccepted {
		if err := store.db.Model(&dopOAuthIdentity{}).Where("user_id = ?", user.ID).Update("initial_pending", true).Error; err != nil {
			writeError(w, r, err)
			return
		}
		// Until approval applies, native quota semantics would mean unlimited.
		// Keep this identity disabled and with no Key. After approval DOP must
		// explicitly enable it, then use api-keys/create with a new operation key.
		user, err = store.disableDOPPendingUser(r.Context(), user)
		if err != nil {
			writeError(w, r, err)
			return
		}
		beforeAudit = view.auditCount
		native.recordAdminAudit(r, actor, "disable_pending_quota", "admin_user", user.ID, "", user)
		if err := view.checkAudit(beforeAudit); err != nil {
			writeError(w, r, err)
			return
		}
		result["user"], result["stage"] = user, "quota_pending"
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	keyBody["owner_user_id"] = user.ID
	beforeAudit = view.auditCount
	keyResult := dopProvisionCall(r, keyBody, func(response http.ResponseWriter, child *http.Request) {
		native.serveAdminProjectKeysPost(response, child, actor, req.ProjectID)
	})
	if keyResult.status >= 400 {
		w.WriteHeader(keyResult.status)
		_, _ = w.Write(keyResult.body.Bytes())
		return
	}
	if err := view.checkAudit(beforeAudit); err != nil {
		writeError(w, r, err)
		return
	}
	result["key"] = json.RawMessage(keyResult.body.Bytes())
	status := http.StatusCreated
	result["stage"] = "ready"
	if keyResult.status == http.StatusAccepted {
		status, result["stage"] = http.StatusAccepted, "key_pending"
	}
	writeJSON(w, status, result)
}

func dopProvisionCall(r *http.Request, body any, handler http.HandlerFunc) *dopBatchResponse {
	encoded, _ := json.Marshal(body)
	child := r.Clone(r.Context())
	child.Body, child.ContentLength = io.NopCloser(bytes.NewReader(encoded)), int64(len(encoded))
	child.Method = http.MethodPost
	response := &dopBatchResponse{header: make(http.Header)}
	handler(response, child)
	return response
}
