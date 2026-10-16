package server

import "time"

// Native handlers build the audit snapshots and metadata. This transaction-
// scoped view preserves the native audit insertion fields while capturing its
// error (Store.RecordAuditEvent has no return value). It avoids per-employee
// audit-table searches and rolls credentials back when audit persistence fails.
type dopProvisionStore struct {
	*dopBatchLookupStore
	auditError error
	auditCount int
}

func (s *dopProvisionStore) RecordAuditEvent(event AuditEvent) {
	if event.ID == "" {
		event.ID = NewID("audit")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.Status == "" {
		event.Status = "success"
	}
	if err := s.db.Create(&event).Error; err != nil {
		s.auditError = err
		return
	}
	s.auditCount++
}

func (s *dopProvisionStore) checkAudit(previous int) error {
	if s.auditError != nil {
		return s.auditError
	}
	if s.auditCount <= previous {
		return NewHTTPError(500, "provision_audit_failed", "Native creation audit could not be persisted")
	}
	return nil
}
