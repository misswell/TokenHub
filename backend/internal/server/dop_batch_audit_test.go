package server

import (
	"net/http"
	"testing"
)

func TestDOPBatchAuditFailureRollsBackWritesAndReceipts(t *testing.T) {
	for _, operation := range []string{"user-quotas", "team-budgets", "api-keys/create"} {
		for _, pending := range []bool{false, true} {
			name := operation + "/applied"
			if pending {
				name = operation + "/pending"
			}
			t.Run(name, func(t *testing.T) {
				store, _, handler := newDOPBatchTestServer(t)
				dopTestUser(t, store, "employee")
				team := store.CreateResource("teams", AdminResource{Name: "Team"})
				project := store.CreateProject(Project{Name: "Project", TeamID: team.ID, Status: StatusActive})
				var item dopBatchItem
				trigger, kind := "quota_increase", "quota-policies"
				switch operation {
				case "user-quotas":
					item = dopTestItem("item", http.MethodPost, "", AdminResource{Name: "Quota", Fields: map[string]any{
						"scope": "user", "scope_id": "employee", "monthly_cost_usd": 100,
					}})
				case "team-budgets":
					trigger, kind = "budget_change", "budgets"
					item = dopTestItem("item", http.MethodPost, "", AdminResource{Name: "Budget", Fields: map[string]any{
						"scope": "team", "scope_id": team.ID, "amount_usd": 100, "enforcement": "hard",
					}})
				case "api-keys/create":
					trigger = "api_key_create"
					item = dopTestItem("item", "", "", map[string]any{
						"project_id": project.ID, "owner_user_id": "employee", "name": "Key", "model_access_mode": "inherit",
					})
				}
				if pending {
					store.CreateResource("approval-flows", AdminResource{Name: "Approval", Status: StatusActive, Fields: map[string]any{"trigger": trigger}})
				}
				// A real SQLite statement failure does not abort the surrounding
				// transaction automatically; the batch must propagate the error.
				if err := store.db.Exec("CREATE TRIGGER dop_audit_failure BEFORE INSERT ON audit_events BEGIN SELECT RAISE(FAIL, 'synthetic audit failure'); END").Error; err != nil {
					t.Fatal(err)
				}
				result := dopBatchResults(t, dopBatchRequest(t, handler, operation, "audit-failure", dopTestItems(item)))[0]
				if result.Status != http.StatusInternalServerError || result.State != "failed" {
					t.Fatalf("audit failure reported success: %+v", result)
				}
				for _, model := range []any{&APIKey{}, &ApprovalRequest{}, &dopBatchReceipt{}, &AuditEvent{}} {
					var count int64
					if err := store.db.Model(model).Count(&count).Error; err != nil || count != 0 {
						t.Fatalf("partial write for %T: count=%d err=%v", model, count, err)
					}
				}
				if len(store.ListResources(kind)) != 0 {
					t.Fatal("resource survived audit failure")
				}
				if err := store.db.Exec("DROP TRIGGER dop_audit_failure").Error; err != nil {
					t.Fatal(err)
				}
				retry := dopBatchResults(t, dopBatchRequest(t, handler, operation, "audit-failure", dopTestItems(item)))[0]
				if retry.Status >= 400 || retry.Replayed {
					t.Fatalf("rolled-back item cannot be retried: %+v", retry)
				}
			})
		}
	}
}
