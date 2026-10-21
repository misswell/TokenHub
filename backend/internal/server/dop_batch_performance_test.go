package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDOPBatchLookupsReadOnlyTargetRows(t *testing.T) {
	for _, operation := range []string{"projects/status", "user-quotas", "team-budgets"} {
		for _, size := range []int{10, 40} {
			t.Run(fmt.Sprintf("%s/%d", operation, size), func(t *testing.T) {
				store, _, handler := newDOPBatchTestServer(t)
				items := make([]dopBatchItem, size)
				for i := range items {
					id := fmt.Sprintf("employee-%d", i)
					dopTestUser(t, store, id)
					team := store.CreateResource("teams", AdminResource{Name: id})
					policy := store.CreateResource("quota-policies", AdminResource{Name: id, Fields: map[string]any{
						"scope": "user", "scope_id": id, "monthly_cost_usd": 100,
					}})
					project := store.CreateProject(Project{Name: id, TeamID: team.ID, DefaultQuotaRef: policy.ID, Status: StatusActive})
					switch operation {
					case "projects/status":
						items[i] = dopTestItem(id, "", project.ID, map[string]any{"status": StatusDisabled})
					case "user-quotas":
						items[i] = dopTestItem(id, http.MethodPatch, policy.ID, map[string]any{"fields": map[string]any{"monthly_cost_usd": 200}})
					case "team-budgets":
						items[i] = dopTestItem(id, http.MethodPost, "", AdminResource{Name: id, Fields: map[string]any{
							"scope": "team", "scope_id": team.ID, "amount_usd": 100, "enforcement": "hard",
						}})
					}
				}
				var scans, projectRows, teamRows int64
				if err := store.db.Callback().Query().After("gorm:query").Register("dop_target_rows", func(db *gorm.DB) {
					if db.Statement.Table == "projects" {
						projectRows += db.RowsAffected
						if !strings.Contains(db.Statement.SQL.String(), "WHERE") {
							scans++
						}
					}
					if db.Statement.Table == "admin_resources" && strings.Contains(fmt.Sprint(db.Statement.Vars), "teams") {
						teamRows += db.RowsAffected
					}
				}); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				response := dopBatchRequest(t, handler, operation, "bounded-lookups", dopTestItems(items...))
				if response.Code != http.StatusOK {
					t.Fatalf("batch failed: %d %s", response.Code, response.Body)
				}
				if scans != 0 || projectRows > int64(5*size) || teamRows > int64(3*size) {
					t.Fatalf("unbounded reads: items=%d project_scans=%d project_rows=%d team_rows=%d", size, scans, projectRows, teamRows)
				}
				t.Logf("items=%d project_scans=%d project_rows=%d team_rows=%d elapsed=%s", size, scans, projectRows, teamRows, time.Since(start))
			})
		}
	}
}

func TestDOPQuotaLookupPreservesApprovalProjectAssociation(t *testing.T) {
	store, app, _ := newDOPBatchTestServer(t)
	dopTestUser(t, store, "employee")
	policy := store.CreateResource("quota-policies", AdminResource{Name: "Quota", Fields: map[string]any{
		"scope": "user", "scope_id": "employee", "monthly_cost_usd": 100,
	}})
	item := dopTestItem("quota", http.MethodPatch, policy.ID, map[string]any{"fields": map[string]any{"monthly_cost_usd": 200}})
	for _, count := range []int{0, 1, 2, 3} {
		if count > 0 {
			project := store.CreateProject(Project{Name: fmt.Sprint(count), DefaultQuotaRef: "\t\u00a0 " + policy.ID + "\r\n"})
			if project.ID == "" {
				t.Fatal("project fixture failed")
			}
		}
		view, err := prepareDOPBatchLookups(store, "user-quotas", item)
		if err != nil {
			t.Fatal(err)
		}
		scoped := &Server{store: view}
		if got, want := scoped.approvalResourceProjectID("quota-policies", policy), app.approvalResourceProjectID("quota-policies", policy); got != want {
			t.Fatalf("%d associations: got %q, want %q", count, got, want)
		}
		if got := len(view.ListProjects()); got > 2 {
			t.Fatalf("loaded %d projects to resolve an ambiguous association", got)
		}
	}
	var plan []struct{ Detail string }
	if err := store.db.Raw("EXPLAIN QUERY PLAN SELECT id, default_quota_ref FROM projects WHERE "+dopQuotaRefExpression(store.db)+" = ? LIMIT 2", policy.ID).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || !strings.Contains(plan[0].Detail, "idx_dop_projects_quota_ref") {
		t.Fatalf("quota lookup does not use its index: %+v", plan)
	}
}
