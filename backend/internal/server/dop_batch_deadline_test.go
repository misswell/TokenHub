package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDOPPostgresConcurrentDeadlineRollsBack(t *testing.T) {
	store, _, handler, _ := newDOPPostgresTestServer(t, DOPBatchOptions{Timeout: time.Second})
	items := dopProvisionItems(t, store, "deadline", 1)
	name := "dop_pg_deadline"
	if err := store.db.Callback().Create().After("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "dop_oauth_identities" {
			<-tx.Statement.Context.Done()
		}
	}); err != nil {
		t.Fatal(err)
	}
	response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "deadline", items, 2))
	if response.Code != 504 {
		t.Fatalf("deadline returned HTTP %d", response.Code)
	}
	if err := store.db.Callback().Create().Remove(name); err != nil {
		t.Fatal(err)
	}
	for _, model := range []any{&dopBatchReceipt{}, &dopOAuthIdentity{}, &APIKey{}} {
		var count int64
		if err := store.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatal("canceled item left partial state")
		}
	}
	var count int64
	if err := store.db.Model(&AdminUser{}).Where("username LIKE ?", "deadline-%").Count(&count).Error; err != nil || count != 0 {
		t.Fatal("canceled item left a user")
	}
}

func TestDOPPostgresReservedCapacityKeepsLegacyAvailable(t *testing.T) {
	reserve := 26
	store, _, handler, _ := newDOPPostgresTestServer(t, DOPBatchOptions{ReservedConnections: &reserve})
	items := dopProvisionItems(t, store, "capacity", 1)
	if response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "capacity", items, 2)); response.Code != 503 {
		t.Fatal("parallel path ignored reserved connections")
	}
	if response := dopWaitHTTP(t, dopConcurrentHTTP(t, handler, "users/provision", "capacity", items, 0)); response.Code != 200 {
		t.Fatal("legacy path was disabled by parallel-only capacity limits")
	}
}

func TestDOPConcurrentCancellationResponse(t *testing.T) {
	api := &dopBatchAPI{}
	r := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()
	api.writeConcurrentError(w, r, context.Canceled, context.Background())
	if w.Code != 503 {
		t.Fatal("internal cancellation was reported as an empty success")
	}
	w = httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api.writeConcurrentError(w, r, context.Canceled, ctx)
	if w.Body.Len() != 0 {
		t.Fatal("response attempted after client cancellation")
	}
}
