package server

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestDOPTransactionReadersObserveUncommittedWrites(t *testing.T) {
	store := newSQLiteIsolationStore(t)
	rollback := errors.New("synthetic item rollback")
	err := store.db.Transaction(func(tx *gorm.DB) error {
		item := store.dopTransactionStore(tx)
		resource := item.CreateResource("teams", AdminResource{Name: "Pending team"})
		if resources := item.ListResources("teams"); len(resources) != 1 || resources[0].ID != resource.ID {
			t.Fatal("native resource lookup escaped the item transaction")
		}
		project := item.CreateProject(Project{Name: "Pending project", TeamID: resource.ID, Status: StatusActive})
		if projects := item.ListProjects(); len(projects) != 1 || projects[0].ID != project.ID {
			t.Fatal("native project lookup escaped the item transaction")
		}
		if projects := store.ListProjects(); len(projects) != 0 {
			t.Fatal("uncommitted project became visible outside the item transaction")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("unexpected transaction error: %v", err)
	}
	if len(store.ListResources("teams")) != 0 || len(store.ListProjects()) != 0 {
		t.Fatal("rolled-back item left partial resources")
	}
}
