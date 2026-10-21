package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A restore is allowed only for an unchanged object disabled by this extension.
// Pre-existing manual disables, expired keys, and changed resources stay disabled.
type dopBatchStatusMark struct {
	Kind        string `gorm:"primaryKey"`
	ResourceID  string `gorm:"primaryKey"`
	Fingerprint string
	AuditID     string
}

func (s *Server) serveDOPBatchStatus(w http.ResponseWriter, r *http.Request, user AdminUser, operation string, item dopBatchItem) {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(item.Body, &body); err != nil || item.ID == "" || (body.Status != StatusActive && body.Status != StatusDisabled) {
		writeError(w, r, NewHTTPError(400, "invalid_batch_status", "id and status=active or disabled are required"))
		return
	}
	store := s.store.(*GormStore)
	project := operation == "projects/status"
	kind := "api_key"
	if project {
		kind = "project"
	}
	// Serialize conflicting requests for the same resource even when they use
	// different idempotency keys. This reuses the native database scope lock.
	if err := store.lockScopeForUpdate(store.db, kind, item.ID); err != nil {
		writeError(w, r, err)
		return
	}
	object, status, err := s.dopBatchStatusObject(project, item.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if status == body.Status {
		writeJSON(w, http.StatusOK, map[string]any{"id": item.ID, "status": status, "changed": false})
		return
	}
	beforeAudit, err := store.dopBatchStatusAuditID(kind, item.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	mark := dopBatchStatusMark{Kind: kind, ResourceID: item.ID}
	if body.Status == StatusActive {
		err := store.db.First(&mark, "kind = ? AND resource_id = ?", kind, item.ID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			writeError(w, r, err)
			return
		}
		encoded, _ := json.Marshal(object)
		if err != nil || status != StatusDisabled || mark.Fingerprint != dopBatchHash(encoded) || mark.AuditID != beforeAudit {
			writeError(w, r, NewHTTPError(409, "restore_not_owned", "Resource was not disabled by DOP or has changed since disable"))
			return
		}
		if key, ok := object.(APIKey); ok && ((key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now())) || key.GraceUntil != nil) {
			writeError(w, r, NewHTTPError(409, "restore_key_expired", "Expired or rotated keys cannot be restored"))
			return
		}
	} else if status != StatusActive {
		writeError(w, r, NewHTTPError(409, "status_not_active", "Only an active resource can be disabled by DOP"))
		return
	}
	patch := any(body)
	if value, ok := object.(Project); ok {
		value.Status = body.Status
		patch = value // Preserve ownership, Team, model access, and quota binding.
	}
	encoded, _ := json.Marshal(patch)
	r.Method, r.Body, r.ContentLength = http.MethodPatch, io.NopCloser(bytes.NewReader(encoded)), int64(len(encoded))
	response := &dopBatchResponse{header: make(http.Header)}
	if project {
		s.serveAdminProjectPatch(response, r, user, item.ID)
	} else {
		s.serveAdminAPIKeyPatch(response, r, user, item.ID)
	}
	if response.status == http.StatusOK {
		afterAudit, err := store.dopBatchStatusAuditID(kind, item.ID)
		if err != nil || afterAudit == "" || beforeAudit == afterAudit {
			writeError(w, r, NewHTTPError(500, "batch_audit_missing", "Status change could not be audited"))
			return
		}
		if body.Status == StatusDisabled {
			updated, _, err := s.dopBatchStatusObject(project, item.ID)
			if err != nil {
				writeError(w, r, err)
				return
			}
			encoded, _ := json.Marshal(updated)
			mark.Fingerprint = dopBatchHash(encoded)
			mark.AuditID = afterAudit
			err = store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&mark).Error
			if err != nil {
				writeError(w, r, err)
				return
			}
		} else if err := store.db.Delete(&mark, "kind = ? AND resource_id = ?", kind, item.ID).Error; err != nil {
			writeError(w, r, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body.Bytes())
}

func (s *GormStore) dopBatchStatusAuditID(kind, id string) (string, error) {
	var event AuditEvent
	err := s.db.Select("id").Where("resource_type = ? AND resource_id = ?", kind, id).
		Order("created_at DESC, id DESC").Take(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return event.ID, err
}

func (s *Server) dopBatchStatusObject(project bool, id string) (any, string, error) {
	if project {
		store := s.store.(*GormStore)
		var value Project
		if err := store.db.First(&value, "id = ?", id).Error; err != nil {
			return value, "", notFound(err, "project_not_found", "Project not found")
		}
		hydrateProject(&value)
		err := store.loadProjectTeams(&value)
		return value, value.Status, err
	}
	value, err := s.findAPIKey(id)
	return value, value.Status, err
}
