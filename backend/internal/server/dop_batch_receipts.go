package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Native models and migration manifests are unchanged. Tombstones prevent an
// expired key from recreating work.
type dopBatchReceipt struct {
	ID          string `gorm:"primaryKey"`
	Fingerprint string
	Ciphertext  string
	CreatedAt   time.Time
}

func dopBatchHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (api *dopBatchAPI) executeItem(r *http.Request, user AdminUser, operation, key string, item dopBatchItem) dopBatchResult {
	result, _ := api.executeItemAttempt(r, user, operation, key, item)
	return result
}

func (api *dopBatchAPI) executeItemAttempt(r *http.Request, user AdminUser, operation, key string, item dopBatchItem) (dopBatchResult, bool) {
	result := dopBatchResult{ClientRef: item.ClientRef, State: "failed"}
	identity, _ := json.Marshal([]string{user.ID, operation, key, item.ClientRef})
	encoded, _ := json.Marshal(item)
	id, fingerprint := dopBatchHash(identity), dopBatchHash(encoded)
	var itemError error
	err := api.store.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		receipt := dopBatchReceipt{ID: id, Fingerprint: fingerprint, CreatedAt: time.Now().UTC()}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&receipt, "id = ?", id).Error; err != nil {
			return err
		}
		if receipt.Fingerprint != fingerprint {
			return NewHTTPError(409, "idempotency_conflict", "This key and client_ref already identify a different item")
		}
		if receipt.Ciphertext != "" {
			if time.Since(receipt.CreatedAt) > 24*time.Hour {
				return NewHTTPError(410, "idempotency_replay_expired", "Replay expired; reconcile existing resources instead of recreating them")
			}
			plaintext := api.store.decryptSecret(receipt.Ciphertext)
			if plaintext == "" || json.Unmarshal([]byte(plaintext), &result) != nil {
				return NewHTTPError(500, "batch_replay_unavailable", "Cannot decrypt the committed result; do not retry with a new key")
			}
			result.Replayed = true
			return nil
		}
		// GormStore contains pointer-owned synchronization. Give this transaction
		// its own method mutex; the database provides cross-process receipt locks.
		store := *api.store
		store.db, store.mu = tx, &sync.Mutex{}
		if err := store.lockDOPBatchTarget(r.Context(), operation, item); err != nil {
			return err
		}
		lookupStore, err := prepareDOPBatchLookups(&store, operation, item)
		if err != nil {
			return err
		}
		var audited *dopProvisionStore
		if view, ok := lookupStore.(*dopBatchLookupStore); ok {
			audited = &dopProvisionStore{dopBatchLookupStore: view}
			lookupStore = audited
		}
		s := &Server{store: lookupStore, config: api.server.config}
		child := r.Clone(r.Context())
		child.Body = io.NopCloser(bytes.NewReader(item.Body))
		child.ContentLength = int64(len(item.Body))
		child.Header = r.Header.Clone()
		child.Header.Set("Content-Type", "application/json")
		response := &dopBatchResponse{header: make(http.Header)}
		api.dispatch(s, response, child, user, operation, item)
		result.Status, result.Body = response.status, append(json.RawMessage(nil), response.body.Bytes()...)
		if result.Status >= http.StatusInternalServerError {
			result.Status, result.Body = dopBatchErrorBody(NewHTTPError(500, "batch_internal_error", "Batch operation failed"))
		}
		if result.Status >= 400 || result.Status == 0 {
			itemError = errors.New("batch item rejected")
			return itemError // Roll back any partial native writes for this item.
		}
		if audited != nil {
			if err := audited.checkAudit(0); err != nil {
				return err
			}
		}
		result.State = "succeeded"
		if result.Status == http.StatusAccepted {
			result.State = "pending"
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		ciphertext, err := api.store.encryptSecretStrict(string(payload))
		if err != nil {
			return err
		}
		return tx.Model(&receipt).Update("ciphertext", ciphertext).Error
	})
	if err != nil && itemError == nil {
		result.Status, result.Body = dopBatchErrorBody(err)
		result.State, result.Replayed = "failed", false
	}
	var state interface{ SQLState() string }
	retry := err != nil && itemError == nil && errors.As(err, &state) && (state.SQLState() == "40P01" || state.SQLState() == "40001")
	return result, retry
}
