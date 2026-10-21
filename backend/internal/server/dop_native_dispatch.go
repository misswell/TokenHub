package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (api *dopBatchAPI) dispatchDOPNative(s *Server, w http.ResponseWriter, r *http.Request, actor AdminUser, operation string, item dopBatchItem) {
	if operation == "native/users/create" {
		s.serveDOPManagedSingleUser(w, r, actor, item)
		return
	}
	if operation == "native/users/bind" {
		s.serveDOPManagedBinding(w, r, actor, item)
		return
	}
	if operation == "native/projects/create" {
		s.handleAdminProjectsPost(w, r)
		return
	}
	kind := strings.TrimPrefix(operation, "native/resource/")
	if item.Method == http.MethodPost {
		s.serveAdminResourceCollectionPost(w, r, actor, kind)
	} else {
		s.serveAdminResourcePatch(w, r, actor, kind, item.ID)
	}
}

func (s *Server) serveDOPManagedSingleUser(w http.ResponseWriter, r *http.Request, actor AdminUser, item dopBatchItem) {
	var req struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		TeamID   string `json:"team_id"`
		Role     string `json:"role"`
		Status   string `json:"status"`
		Provider string `json:"dop_provider"`
		Subject  string `json:"dop_subject"`
	}
	if err := json.Unmarshal(item.Body, &req); err != nil {
		writeError(w, r, NewHTTPError(400, "invalid_user", "Invalid user"))
		return
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(item.Body, &raw)
	for field := range raw {
		switch field {
		case "username", "name", "email", "team_id", "role", "status", "dop_provider", "dop_subject":
		default:
			writeError(w, r, NewHTTPError(400, "invalid_user", "Password and protected fields are not accepted"))
			return
		}
	}
	if req.Role != "user" || req.Status != StatusDisabled || req.TeamID == "" || req.Subject == "" {
		writeError(w, r, NewHTTPError(400, "invalid_user", "Managed single users must start disabled with a Team and bound OAuth identity"))
		return
	}
	if _, ok := s.findActiveOAuthIdentityProvider(req.Provider); !ok {
		writeError(w, r, NewHTTPError(400, "invalid_oauth_provider", "An active provider is required"))
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Username == "" || req.Email == "" || len(req.Username) > 128 || len(req.Email) > 254 || len(req.Name) > 256 {
		writeError(w, r, NewHTTPError(400, "invalid_user", "A bounded username and email are required"))
		return
	}
	store := s.store.(*GormStore)
	user, err := store.CreateAdminUser(AdminUser{Username: req.Username, Email: req.Email, Name: req.Name, Role: "user", TeamID: req.TeamID, Status: StatusDisabled, PasswordHash: dopOAuthOnlyPassword}, "")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err = store.db.Create(&dopOAuthIdentity{UserID: user.ID, ProviderID: req.Provider, Subject: req.Subject, CreatedAt: time.Now().UTC(), InitialPending: true}).Error; err != nil {
		writeError(w, r, writeConflict(err, "oauth_identity_conflict", "Identity is already bound"))
		return
	}
	view := &dopProvisionStore{dopBatchLookupStore: &dopBatchLookupStore{GormStore: store}}
	native := &Server{store: view, config: s.config}
	native.recordAdminAudit(r, actor, "create", "admin_user", user.ID, "", user)
	if err = view.checkAudit(0); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}
