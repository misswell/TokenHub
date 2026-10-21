package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"gorm.io/gorm"
)

func TestDOPProvisionRejectsCredentialAndScopeOverrides(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	fixture := dopProvisionFixture(t, store, "employee")
	for _, field := range []string{"password", "password_hash", "role", "status", "id", "team_ids"} {
		t.Run(field, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal(fixture.Body, &body); err != nil {
				t.Fatal(err)
			}
			body[field] = "admin"
			item := dopTestItem(field, "", "", body)
			if result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", field, dopTestItems(item)))[0]; result.Status != 400 {
				t.Fatalf("protected field was accepted: %s", result.Body)
			}
		})
	}
	var body dopProvisionRequest
	if err := json.Unmarshal(fixture.Body, &body); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*dopProvisionRequest){
		func(v *dopProvisionRequest) { v.MonthlyLimitUSD = 0 },
		func(v *dopProvisionRequest) { v.TeamID = "other-team" },
		func(v *dopProvisionRequest) { v.OAuthSubject = "" },
		func(v *dopProvisionRequest) { v.OAuthProviderID = "other-provider" },
		func(v *dopProvisionRequest) { v.Key = json.RawMessage(`{"owner_user_id":"dop-admin"}`) },
	} {
		candidate := body
		change(&candidate)
		item := dopTestItem("invalid", "", "", candidate)
		if result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "invalid", dopTestItems(item)))[0]; result.Status != 400 {
			t.Fatalf("invalid employee accepted: %s", result.Body)
		}
	}
	if len(store.ListAdminUsers()) != 1 || len(store.ListProjectKeys("dop-project")) != 0 {
		t.Fatal("invalid inputs created employees or Keys")
	}
}

func TestDOPProvisionAuditFailureRollsBackCredentials(t *testing.T) {
	store, _, handler := newDOPBatchTestServer(t)
	item := dopProvisionFixture(t, store, "employee")
	if err := store.db.Callback().Create().Before("gorm:create").Register("dop_reject_key_audit", func(db *gorm.DB) {
		if event, ok := db.Statement.Dest.(*AuditEvent); ok && event.ResourceType == "api_key" {
			_ = db.AddError(errors.New("synthetic audit storage failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "audit-failure", dopTestItems(item)))[0]
	if result.Status != http.StatusInternalServerError || len(store.ListAdminUsers()) != 1 || len(store.ListResources("quota-policies")) != 0 || len(store.ListProjectKeys("dop-project")) != 0 {
		t.Fatal("audit failure left usable employee credentials")
	}
}
