package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
)

func (s *GormStore) validateDOPProvision(server *Server, r *http.Request, item dopBatchItem) (dopProvisionRequest, map[string]any, error) {
	var req dopProvisionRequest
	invalid := func(message string) (dopProvisionRequest, map[string]any, error) {
		return req, nil, NewHTTPError(400, "invalid_employee_provision", message)
	}
	decoder := json.NewDecoder(bytes.NewReader(item.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalid("Invalid body; password, role and existing user IDs are not accepted")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("Body must contain exactly one object")
	}
	if item.ID != "" || item.Method != "" {
		return invalid("Provision items must omit method and id")
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.Username == "" {
		req.Username = req.Email
	}
	if req.Email == "" || len(req.Email) > 254 || len(req.Username) > 128 || len(req.Name) > 256 || strings.TrimSpace(req.TeamID) == "" || req.ProjectID == "" || req.OAuthProviderID == "" || len(req.OAuthProviderID) > 128 || req.OAuthSubject == "" || len(req.OAuthSubject) > 512 || req.MonthlyLimitUSD <= 0 || math.IsNaN(req.MonthlyLimitUSD) || math.IsInf(req.MonthlyLimitUSD, 0) {
		return invalid("email, team_id, project_id, oauth_provider_id, exact oauth_subject (OIDC sub) and a positive monthly_limit_usd are required")
	}
	if _, ok := server.findActiveOAuthIdentityProvider(req.OAuthProviderID); !ok {
		return invalid("oauth_provider_id must identify an active configured OAuth provider")
	}
	var project Project
	if err := s.db.First(&project, "id = ?", req.ProjectID).Error; err != nil {
		return req, nil, notFound(err, "project_not_found", "Project not found")
	}
	if project.TeamID != req.TeamID || project.Status != StatusActive {
		return invalid("Project must be active and its primary Team must match team_id")
	}
	existing, ok := r.Context().Value(dopProvisionContextKey{}).(dopProvisionExisting)
	if !ok {
		return req, nil, NewHTTPError(500, "provision_preflight_missing", "Employee identity preflight is required")
	}
	if existing.usernames[req.Username] || existing.emails[req.Email] {
		return req, nil, NewHTTPError(409, "admin_user_conflict", "Username or email already exists; existing users are never automatically linked")
	}
	var key map[string]any
	if err := json.Unmarshal(req.Key, &key); err != nil || key == nil {
		return invalid("key must be an object containing native Key creation parameters")
	}
	allowed := map[string]bool{"name": true, "group": true, "allowed_models": true, "model_access_mode": true, "ip_allowlist": true, "limits": true, "rate_limit_rpm": true, "token_limit_tpm": true, "expires_at": true}
	for field := range key {
		if !allowed[field] {
			return invalid("Unknown or protected Key field: " + field)
		}
	}
	return req, key, nil
}
