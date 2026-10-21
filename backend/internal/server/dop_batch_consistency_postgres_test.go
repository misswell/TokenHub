package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDOPPostgresSerialRequestsPreserveIndependentFields(t *testing.T) {
	for _, operation := range []string{"user-quotas", "team-budgets"} {
		t.Run(operation, func(t *testing.T) {
			store, _, handler, _ := newDOPPostgresTestServer(t)
			dopTestUser(t, store, "employee")
			kind, amount := "quota-policies", "monthly_cost_usd"
			fields := map[string]any{"scope": "user", "scope_id": "employee", amount: 100, "daily_requests": 10}
			if operation == "team-budgets" {
				team := store.CreateResource("teams", AdminResource{Name: "Team"})
				kind, amount = "budgets", "amount_usd"
				fields = map[string]any{"scope": "team", "scope_id": team.ID, amount: 100, "enforcement": "hard", "daily_requests": 10}
			}
			resource := store.CreateResource(kind, AdminResource{Name: "Resource", Fields: fields})
			firstRead, secondRead, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			if err := store.db.Callback().Query().After("gorm:query").Register("dop_serial_fields", func(db *gorm.DB) {
				row, ok := db.Statement.Dest.(*AdminResource)
				if !ok || row.ID != resource.ID || strings.Contains(db.Statement.SQL.String(), "FOR UPDATE") {
					return
				}
				switch reads.Add(1) {
				case 1:
					close(firstRead)
					<-release
				case 2:
					close(secondRead)
				}
			}); err != nil {
				t.Fatal(err)
			}
			responses := make(chan *httptest.ResponseRecorder, 2)
			request := func(key string, patch map[string]any) {
				item := dopTestItem("resource", http.MethodPatch, resource.ID, map[string]any{"fields": patch})
				responses <- dopBatchRequest(t, handler, operation, key, dopTestItems(item))
			}
			go request("amount", map[string]any{amount: 300})
			select {
			case <-firstRead:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("first request did not reach its resource lookup")
			}
			go request("daily", map[string]any{"daily_requests": 200})
			// Observe the second transaction actually waiting on a PostgreSQL
			// lock, rather than relying on scheduling delays to cause overlap.
			blocked := false
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				var count int64
				err := store.db.Raw("SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&count).Error
				if err != nil {
					t.Error(err)
					break
				}
				if count > 0 {
					blocked = true
					break
				}
				select {
				case <-secondRead:
					until = time.Time{}
				case <-time.After(10 * time.Millisecond):
				}
			}
			close(release)
			for range 2 {
				select {
				case response := <-responses:
					if response.Code != http.StatusOK {
						t.Errorf("request failed: %d %s", response.Code, response.Body)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("serial requests did not finish")
				}
			}
			var updated AdminResource
			if err := store.db.First(&updated, "kind = ? AND id = ?", kind, resource.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !blocked || float64Field(updated.Fields, amount) != 300 || float64Field(updated.Fields, "daily_requests") != 200 {
				t.Fatalf("lost update: blocked=%v fields=%v", blocked, updated.Fields)
			}
		})
	}
}

func TestDOPPostgresTeamUsageKeepsOneSnapshot(t *testing.T) {
	store, _, _, _ := newDOPPostgresTestServer(t)
	team := store.CreateResource("teams", AdminResource{Name: "Team"})
	project := store.CreateProject(Project{Name: "Project", TeamID: team.ID, Status: StatusActive})
	budget := store.CreateResource("budgets", AdminResource{Name: "Budget", Fields: map[string]any{
		"scope": "team", "scope_id": team.ID, "amount_usd": 100, "enforcement": "hard",
	}})
	now := time.Now().UTC()
	usage := UsageRecord{ID: NewID("usage"), RequestID: NewID("req"), ProjectID: project.ID, CostUSD: 10, CreatedAt: now}
	if err := store.db.Create(&usage).Error; err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	if err := store.db.Callback().Query().After("gorm:query").Register("dop_team_snapshot", func(db *gorm.DB) {
		if db.Statement.Table != "admin_resources" || !strings.Contains(fmt.Sprint(db.Statement.Vars), "budgets") || !changed.CompareAndSwap(false, true) {
			return
		}
		err := store.db.Transaction(func(other *gorm.DB) error {
			budget.Fields["amount_usd"] = float64(200)
			if err := other.Save(&budget).Error; err != nil {
				return err
			}
			return other.Model(&usage).Update("cost_usd", 40).Error
		})
		if err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][2]float64{{100, 10}, {200, 40}} {
		results, _, err := store.readDOPBatchTeamUsage(context.Background(), []string{team.ID})
		if err != nil || len(results) != 1 {
			t.Fatalf("read failed: %v", err)
		}
		item := results[0]
		if !changed.Load() || item.MonthlyLimitUSD == nil || item.UsedUSD == nil || *item.MonthlyLimitUSD != expected[0] || *item.UsedUSD != expected[1] {
			t.Fatalf("mixed snapshot: %+v, want %v", item, expected)
		}
	}
}

func TestDOPPostgresUserUsageKeepsOneSnapshot(t *testing.T) {
	store, _, _, _ := newDOPPostgresTestServer(t)
	dopTestUser(t, store, "employee")
	periods, err := currentQuotaPeriods(store.db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	durable := QuotaBucket{KeyID: userQuotaBucketKey("employee"), Scope: "month", Bucket: periods.Month, AttributedUserID: "employee", QuotaCounter: QuotaCounter{CostUSD: 10}}
	attributed := durable
	attributed.KeyID = "employee-key"
	for _, bucket := range []*QuotaBucket{&durable, &attributed} {
		if err := store.db.Create(bucket).Error; err != nil {
			t.Fatal(err)
		}
	}
	var changed atomic.Bool
	if err := store.db.Callback().Query().After("gorm:query").Register("dop_user_snapshot", func(db *gorm.DB) {
		if db.Statement.Table != "quota_buckets" || !changed.CompareAndSwap(false, true) {
			return
		}
		if err := store.db.Model(&QuotaBucket{}).Where("attributed_user_id = ?", "employee").Update("cost_usd", 40).Error; err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []float64{10, 40} {
		results, _, _, err := store.readDOPBatchUsage(context.Background(), []dopBatchUsageInput{{UserID: "employee"}})
		if err != nil || len(results) != 1 {
			t.Fatalf("read failed: %v", err)
		}
		if !changed.Load() || results[0].CurrentUsage.Monthly.CostUSD != expected {
			t.Fatalf("mixed user counters: %+v, want %v", results[0], expected)
		}
	}
}
