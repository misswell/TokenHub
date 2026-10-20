package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"io"
	"net/http"
	"time"
)

// Managed employee state uses native identity validation while tracking DOP stop
// ownership. Disabling an employee also prevents issuing replacement Keys.
func (s *Server) serveDOPManagedUserStatus(w http.ResponseWriter, r *http.Request, actor AdminUser, item dopBatchItem) {
	store := s.store.(*GormStore)
	var patch struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(item.Body, &patch) != nil || item.ID == "" || (patch.Status != StatusActive && patch.Status != StatusDisabled) {
		writeError(w, r, NewHTTPError(400, "invalid_user_status", "An employee ID and active/disabled status are required"))
		return
	}
	var user AdminUser
	if err := store.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", item.ID).First(&user).Error; err != nil {
		writeError(w, r, notFound(err, "user_not_found", "User not found"))
		return
	}
	var binding dopOAuthIdentity
	if err := store.db.Where("user_id = ?", user.ID).First(&binding).Error; err != nil || user.Role != "user" {
		writeError(w, r, NewHTTPError(403, "managed_employee_required", "Only managed employees can use this state operation"))
		return
	}
	if user.Status == patch.Status {
		writeJSON(w, 200, map[string]any{"id": user.ID, "status": user.Status, "changed": false})
		return
	}
	before, err := store.dopBatchStatusAuditID("admin_user", user.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	mark := dopBatchStatusMark{Kind: "admin_user", ResourceID: user.ID}
	if patch.Status == StatusActive {
		if binding.InitialPending {
			var policies []AdminResource
			scope := statementJSON(store.db, "fields", "scope")
			id := statementJSON(store.db, "fields", "scope_id")
			err = store.db.Where("kind = ? AND status = ?", "quota-policies", StatusActive).Where(scope+" = ? AND "+id+" = ?", "user", user.ID).Find(&policies).Error
			valid := false
			for _, policy := range policies {
				if float64Field(policy.Fields, "monthly_cost_usd") > 0 {
					valid = true
				}
			}
			if err != nil || !valid {
				writeError(w, r, NewHTTPError(409, "employee_quota_pending", "A positive active user quota must exist before activation"))
				return
			}
		} else {
			err = store.db.First(&mark, "kind = ? AND resource_id = ?", mark.Kind, user.ID).Error
			encoded, _ := json.Marshal(publicAdminUser(user))
			if err != nil || mark.Fingerprint != dopBatchHash(encoded) || mark.AuditID != before {
				writeError(w, r, NewHTTPError(409, "restore_not_owned", "Employee was not disabled by DOP or has changed"))
				return
			}
		}
	}
	body, err := json.Marshal(AdminUser{Status: patch.Status, TeamID: user.TeamID, TeamIDs: user.TeamIDs})
	if err != nil {
		writeError(w, r, err)
		return
	}
	child := r.Clone(r.Context())
	child.Method = http.MethodPatch
	child.URL.Path = "/api/admin/users/" + user.ID
	child.Body = io.NopCloser(bytes.NewReader(body))
	child.ContentLength = int64(len(body))
	native := &Server{store: &dopIdentityStore{GormStore: store}, config: s.config}
	response := &dopBatchResponse{header: make(http.Header)}
	native.handleAdminUserItem(response, child)
	if response.status != 200 {
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
		return
	}
	var updated AdminUser
	if err = store.db.First(&updated, "id = ?", user.ID).Error; err != nil {
		writeError(w, r, err)
		return
	}
	after, err := store.dopBatchStatusAuditID("admin_user", user.ID)
	if err != nil || before == after || after == "" {
		writeError(w, r, NewHTTPError(500, "batch_audit_missing", "Employee state could not be audited"))
		return
	}
	if patch.Status == StatusDisabled {
		encoded, _ := json.Marshal(publicAdminUser(updated))
		mark.Fingerprint = dopBatchHash(encoded)
		mark.AuditID = after
		err = store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&mark).Error
	} else {
		err = store.db.Where("kind = ? AND resource_id = ?", mark.Kind, user.ID).Delete(&dopBatchStatusMark{}).Error
		if err == nil {
			err = store.db.Model(&binding).Update("initial_pending", false).Error
		}
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, publicAdminUser(updated))
}

func (s *Server) serveDOPManagedBinding(w http.ResponseWriter, r *http.Request, actor AdminUser, item dopBatchItem) {
	var request struct {
		Provider string `json:"dop_provider"`
		Subject  string `json:"dop_subject"`
	}
	if json.Unmarshal(item.Body, &request) != nil || request.Provider == "" || request.Subject == "" {
		writeError(w, r, NewHTTPError(400, "invalid_identity", "An exact trusted identity is required"))
		return
	}
	if _, ok := s.findActiveOAuthIdentityProvider(request.Provider); !ok {
		writeError(w, r, NewHTTPError(400, "invalid_oauth_provider", "Active provider required"))
		return
	}
	store := s.store.(*GormStore)
	var user AdminUser
	if err := store.db.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", item.ID).Error; err != nil {
		writeError(w, r, notFound(err, "user_not_found", "User not found"))
		return
	}
	if user.Role != "user" {
		writeError(w, r, NewHTTPError(403, "employee_required", "Administrators cannot be converted"))
		return
	}
	var binding dopOAuthIdentity
	err := store.db.First(&binding, "user_id = ?", user.ID).Error
	noLogin := err == nil && binding.ProviderID == dopNoLoginProvider
	if err == nil && !noLogin {
		if binding.ProviderID != request.Provider || binding.Subject != request.Subject {
			writeError(w, r, NewHTTPError(409, "immutable_identity", "Existing identity cannot be rebound"))
			return
		}
		writeJSON(w, 200, publicAdminUser(user))
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, r, err)
		return
	}
	initialPending := binding.InitialPending
	binding = dopOAuthIdentity{UserID: user.ID, ProviderID: request.Provider, Subject: request.Subject, CreatedAt: time.Now().UTC(), InitialPending: initialPending}
	if noLogin {
		err = store.db.Model(&dopOAuthIdentity{}).Where("user_id = ? AND provider_id = ?", user.ID, dopNoLoginProvider).
			Updates(map[string]any{"provider_id": request.Provider, "subject": request.Subject}).Error
	} else {
		err = store.db.Create(&binding).Error
	}
	if err != nil {
		writeError(w, r, writeConflict(err, "identity_conflict", "Identity already bound"))
		return
	}
	if err = store.db.Model(&user).Update("password_hash", dopOAuthOnlyPassword).Error; err != nil {
		writeError(w, r, err)
		return
	}
	if err = store.db.Where("user_id = ?", user.ID).Delete(&AdminSession{}).Error; err != nil {
		writeError(w, r, err)
		return
	}
	view := &dopProvisionStore{dopBatchLookupStore: &dopBatchLookupStore{GormStore: store}}
	native := &Server{store: view, config: s.config}
	native.recordAdminAudit(r, actor, "bind_oauth_identity", "admin_user", user.ID, "", publicAdminUser(user))
	if err = view.checkAudit(0); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, publicAdminUser(user))
}
