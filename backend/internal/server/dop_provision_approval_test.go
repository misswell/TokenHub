package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestDOPProvisionPreservesApprovalsWithoutEarlyKeys(t *testing.T) {
	for _, trigger := range []string{"quota_increase", "api_key_create"} {
		t.Run(trigger, func(t *testing.T) {
			store, app, handler := newDOPBatchTestServer(t)
			item := dopProvisionFixture(t, store, "employee")
			store.CreateResource("approval-flows", AdminResource{Name: "Required", Status: StatusActive, Fields: map[string]any{"trigger": trigger}})
			response := dopBatchRequest(t, handler, "users/provision", "approval", dopTestItems(item))
			result := dopBatchResults(t, response)[0]
			var value dopProvisionTestResult
			if response.Code != 207 || result.Status != 202 || result.State != "pending" || json.Unmarshal(result.Body, &value) != nil || value.Key.Secret != "" || len(store.ListProjectKeys("dop-project")) != 0 {
				t.Fatalf("approval was bypassed or reported as applied: %d %s", response.Code, response.Body)
			}
			if trigger == "quota_increase" && (value.Stage != "quota_pending" || value.User.Status != StatusDisabled || len(store.ListResources("quota-policies")) != 0) {
				t.Fatal("pending quota left an active unlimited employee")
			}
			if trigger == "api_key_create" && (value.Stage != "key_pending" || value.User.Status != StatusActive || len(store.ListResources("quota-policies")) != 1) {
				t.Fatal("Key approval ran before its quota was applied")
			}
			replay := dopBatchResults(t, dopBatchRequest(t, handler, "users/provision", "approval", dopTestItems(item)))[0]
			if !replay.Replayed || len(store.ListApprovalRequests()) != 1 {
				t.Fatal("retry duplicated approval requests")
			}
			actor := store.ListAdminUsers()[0]
			// Native approval execution must still be possible while a newly
			// provisioned employee is disabled pending its first quota.
			if _, err := app.applyApprovalRequest(store.ListApprovalRequests()[0], actor); err != nil {
				t.Fatalf("native approval could not be applied: %v", err)
			}
			if trigger == "quota_increase" {
				if len(store.ListResources("quota-policies")) != 1 || len(store.ListProjectKeys("dop-project")) != 0 {
					t.Fatal("quota approval unexpectedly created a Key")
				}
				enable := doJSON(t, handler, http.MethodPatch, "/api/admin/users/"+value.User.ID, map[string]any{"status": StatusActive, "team_id": "dop-team"}, "")
				if enable.Code != 200 {
					t.Fatal("could not enable employee after quota approval")
				}
				key := dopTestItem("employee-key", "", "", map[string]any{"project_id": "dop-project", "owner_user_id": value.User.ID, "name": "Approved employee", "model_access_mode": "inherit"})
				if created := dopBatchRequest(t, handler, "api-keys/create", "after-approval", dopTestItems(key)); created.Code != 200 {
					t.Fatalf("could not resume after approval: %s", created.Body)
				}
			}
		})
	}
}
