package server

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestDOPPostgresProvisionLocksPreventMutation(t *testing.T) {
	store, _, handler, _ := newDOPPostgresTestServer(t)
	items := dopProvisionItems(t, store, "protected", 1)
	entered, barrier := make(chan struct{}, 1), make(chan struct{})
	name := "dop_pg_protected"
	if err := store.db.Callback().Create().After("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "dop_oauth_identities" {
			entered <- struct{}{}
			select {
			case <-barrier:
			case <-tx.Statement.Context.Done():
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	done := dopConcurrentHTTP(t, handler, "users/provision", "protected", items, 2)
	<-entered
	for _, model := range []any{&AdminResource{}, &Project{}} {
		err := store.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL lock_timeout = '100ms'").Error; err != nil {
				return err
			}
			if _, ok := model.(*AdminResource); ok {
				return tx.Model(model).Where("kind = ? AND id = ?", "teams", "dop-team").Update("status", StatusDisabled).Error
			}
			return tx.Model(model).Where("id = ?", "dop-project").Update("status", StatusDisabled).Error
		})
		var state interface{ SQLState() string }
		if !errors.As(err, &state) || state.SQLState() != "55P03" {
			t.Error("provisioning did not protect Team or Project against concurrent updates")
		}
	}
	close(barrier)
	if response := dopWaitHTTP(t, done); response.Code != 200 {
		t.Fatal("protected provisioning failed")
	}
	if err := store.db.Callback().Create().Remove(name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateResource("teams", "dop-team", AdminResource{Status: StatusDisabled}); err != nil {
		t.Fatal(err)
	}
	// Reuse the body with a new identity without recreating/re-enabling fixtures.
	var req dopProvisionRequest
	if err := store.db.First(&AdminResource{}, "kind = ? AND id = ?", "teams", "dop-team").Error; err != nil {
		t.Fatal(err)
	}
	req.Username, req.Email, req.OAuthSubject = "inactive", "inactive@example.test", "inactive"
	req.TeamID, req.ProjectID, req.OAuthProviderID, req.MonthlyLimitUSD = "dop-team", "dop-project", "dop-oidc", 100
	req.Key = []byte(`{"name":"Employee","model_access_mode":"inherit"}`)
	response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "inactive", []dopBatchItem{dopTestItem("inactive", "", "", req)}, 2))
	if response.Code != 207 || dopBatchResults(t, response)[0].Status != 400 {
		t.Fatal("inactive Team accepted a parallel employee")
	}
	var count int64
	if err := store.db.Model(&AdminUser{}).Where("username = ?", "inactive").Count(&count).Error; err != nil || count != 0 {
		t.Fatal("inactive Team left an employee")
	}
	// No blocked transaction or worker may survive the completed request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.(*dopBatchHTTPHandler).Shutdown(ctx); err != nil && err != context.Canceled {
		t.Fatal(err)
	}
}
