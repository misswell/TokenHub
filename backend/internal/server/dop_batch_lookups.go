package server

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

// Native amount/key handlers locate a known owner or resource by scanning their
// Store's lists. This item-scoped view supplies those exact lookup candidates,
// avoiding repeated full-table reads for large platform-admin batches. Every
// other Store operation, including authorization, approval and audit storage,
// remains native. Never use this view for general collection endpoints.
type dopBatchLookupStore struct {
	*GormStore
	filterUsers bool
	users       []AdminUser
	kind        string
	resources   []AdminResource
}

func (s *dopBatchLookupStore) ListAdminUsers() []AdminUser {
	if s.filterUsers {
		return s.users
	}
	return s.GormStore.ListAdminUsers()
}

func (s *dopBatchLookupStore) ListResources(kind string) []AdminResource {
	if kind == s.kind {
		return s.resources
	}
	return s.GormStore.ListResources(kind)
}

func prepareDOPBatchLookups(store *GormStore, operation string, item dopBatchItem) (Store, error) {
	if operation == "native/resource/quota-policies" {
		operation = "user-quotas"
	}
	if operation == "native/resource/budgets" {
		operation = "team-budgets"
	}
	if operation != "user-quotas" && operation != "team-budgets" && operation != "api-keys/create" {
		return store, nil
	}
	var body struct {
		OwnerUserID string         `json:"owner_user_id"`
		Fields      map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(item.Body, &body); err != nil {
		return nil, NewHTTPError(400, "invalid_batch_item", "Invalid item body")
	}
	view := &dopBatchLookupStore{GormStore: store, filterUsers: operation != "team-budgets"}
	userID := body.OwnerUserID
	if operation == "user-quotas" {
		userID = stringField(body.Fields, "scope_id")
	}
	if item.ID != "" && (operation == "user-quotas" || operation == "team-budgets") {
		view.kind = "quota-policies"
		if operation == "team-budgets" {
			view.kind = "budgets"
		}
		var resource AdminResource
		err := store.db.Where("kind = ? AND id = ?", view.kind, item.ID).Take(&resource).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, NewHTTPError(404, "resource_not_found", "Resource not found")
		}
		if err != nil {
			return nil, err
		}
		view.resources = []AdminResource{resource}
		if operation == "user-quotas" {
			userID = stringField(resource.Fields, "scope_id")
		}
	}
	if view.filterUsers && userID != "" {
		if err := store.db.Where("id = ?", userID).Find(&view.users).Error; err != nil {
			return nil, err
		}
		for index := range view.users {
			view.users[index] = publicAdminUser(view.users[index])
			// Native quota validation requires an active owner. A DOP-owned
			// suspended employee remains disabled in storage until the positive
			// quota has committed; only this validation view sees it as active.
			if operation == "user-quotas" && view.users[index].Status == StatusDisabled {
				allowed, err := store.dopPendingQuotaOwner(view.users[index])
				if err != nil {
					return nil, err
				}
				if allowed {
					view.users[index].Status = StatusActive
				}
			}
		}
	}
	return view, nil
}

func (s *GormStore) dopPendingQuotaOwner(user AdminUser) (bool, error) {
	if user.Role != "user" {
		return false, nil
	}
	var binding dopOAuthIdentity
	err := s.db.First(&binding, "user_id = ?", user.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if binding.InitialPending {
		return true, nil
	}
	var mark dopBatchStatusMark
	err = s.db.First(&mark, "kind = ? AND resource_id = ?", "admin_user", user.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	audit, err := s.dopBatchStatusAuditID("admin_user", user.ID)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(publicAdminUser(user))
	if err != nil {
		return false, err
	}
	return mark.Fingerprint == dopBatchHash(encoded) && mark.AuditID == audit, nil
}
