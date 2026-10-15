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
		}
	}
	return view, nil
}
