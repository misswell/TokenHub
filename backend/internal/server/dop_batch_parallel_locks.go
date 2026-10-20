package server

import (
	"context"
	"encoding/json"
	"sort"

	"gorm.io/gorm/clause"
)

func dopBatchParallel(ctx context.Context) (dopBatchParallelMode, bool) {
	mode, ok := ctx.Value(dopBatchParallelKey{}).(dopBatchParallelMode)
	return mode, ok
}

// Lock before item-scoped lookups cache the old Fields value.
func (s *GormStore) lockDOPBatchTarget(ctx context.Context, operation string, item dopBatchItem) error {
	if _, ok := dopBatchParallel(ctx); !ok || s.dbDriver != "postgres" {
		return nil
	}
	if (operation == "user-quotas" || operation == "team-budgets") && item.ID != "" {
		kind := "quota-policies"
		if operation == "team-budgets" {
			kind = "budgets"
		}
		var resource AdminResource
		if err := s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("kind = ? AND id = ?", kind, item.ID).First(&resource).Error; err != nil {
			return notFound(err, "resource_not_found", "Resource not found")
		}
	}
	if operation == "api-keys/create" {
		var body struct {
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal(item.Body, &body) == nil && body.ProjectID != "" {
			if err := s.lockScopeForSharedRead(s.db, "project", body.ProjectID); err != nil {
				return err
			}
			var project Project
			if err := s.db.Clauses(clause.Locking{Strength: "SHARE"}).First(&project, "id = ?", body.ProjectID).Error; err != nil {
				return notFound(err, "project_not_found", "Project not found")
			}
		}
	}
	return nil
}

func (s *GormStore) createDOPProvisionUser(ctx context.Context, req dopProvisionRequest) (AdminUser, error) {
	user := AdminUser{Username: req.Username, Name: req.Name, Email: req.Email, Role: "user", TeamID: req.TeamID, Status: StatusActive, PasswordHash: dopOAuthOnlyPassword}
	mode, parallel := dopBatchParallel(ctx)
	if !parallel || !mode.provision || s.dbDriver != "postgres" {
		return s.CreateAdminUser(user, "")
	}
	keys := dopBatchIdentityKeys(req)
	sort.Strings(keys)
	for _, key := range keys {
		if err := s.lockScopeForUpdate(s.db, "dop_identity", key); err != nil {
			return AdminUser{}, err
		}
	}
	var team AdminResource
	if err := s.db.Clauses(clause.Locking{Strength: "SHARE"}).Where("kind = ? AND id = ?", "teams", req.TeamID).First(&team).Error; err != nil {
		return AdminUser{}, notFound(err, "team_not_found", "Team not found")
	}
	if team.Status != "" && team.Status != StatusActive {
		return AdminUser{}, NewHTTPError(400, "team_inactive", "Only an active team can be assigned to a user")
	}
	var project Project
	if err := s.db.Clauses(clause.Locking{Strength: "SHARE"}).First(&project, "id = ?", req.ProjectID).Error; err != nil {
		return AdminUser{}, notFound(err, "project_not_found", "Project not found")
	}
	if project.Status != StatusActive || project.TeamID != req.TeamID {
		return AdminUser{}, NewHTTPError(400, "invalid_employee_provision", "Project must be active and its primary Team must match team_id")
	}
	return createAdminUser(s.db, user, "")
}

func (s *GormStore) disableDOPPendingUser(ctx context.Context, user AdminUser) (AdminUser, error) {
	patch := AdminUser{Status: StatusDisabled, TeamID: user.TeamID, TeamIDs: user.TeamIDs}
	mode, parallel := dopBatchParallel(ctx)
	if parallel && mode.provision && s.dbDriver == "postgres" {
		// This newly inserted user and its Team are already protected. Reentering
		// UpdateAdminUser would upgrade shared Team locks and can deadlock peers.
		return updateAdminUser(s.db, user.ID, patch, "")
	}
	return s.UpdateAdminUser(user.ID, patch, "")
}
