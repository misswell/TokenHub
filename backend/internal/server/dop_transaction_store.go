package server

import (
	"sync"

	"gorm.io/gorm"
)

func (s *GormStore) dopTransactionStore(tx *gorm.DB) *GormStore {
	store := *s
	// Native readers must observe this item's uncommitted writes and locks.
	// Dedicated read pools would otherwise escape the receipt transaction.
	store.db, store.readDB, store.analyticsDB = tx, tx, tx
	store.mu = &sync.Mutex{}
	return &store
}
