package server

import (
	"context"
	"net/http"
	"strings"
)

type dopProvisionContextKey struct{}

type dopProvisionExisting struct {
	usernames map[string]bool
	emails    map[string]bool
}

// Native uniqueness checks use indexed, case-sensitive columns. One snapshot
// protects pre-existing mixed-case identities without 20000 LOWER table scans.
// The native unique indexes still arbitrate concurrent normalized creates.
func (api *dopBatchAPI) provisionPreflight(r *http.Request) (*http.Request, error) {
	var users []AdminUser
	if err := api.store.db.WithContext(r.Context()).Select("username", "email").Find(&users).Error; err != nil {
		return r, err
	}
	existing := dopProvisionExisting{usernames: make(map[string]bool, len(users)), emails: make(map[string]bool, len(users))}
	for _, user := range users {
		existing.usernames[strings.ToLower(user.Username)] = true
		existing.emails[strings.ToLower(user.Email)] = true
	}
	return r.WithContext(context.WithValue(r.Context(), dopProvisionContextKey{}, existing)), nil
}
