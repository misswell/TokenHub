package server

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"
)

type dopBatchParallelKey struct{}
type dopBatchParallelMode struct{ provision bool }

func (api *dopBatchAPI) serveConcurrent(w http.ResponseWriter, r *http.Request, user AdminUser, operation, key string, items []dopBatchItem, workers int) {
	e := api.executor
	parent := r.Context()
	ctx, releaseRequest, err := e.admit(r.Context())
	if err != nil {
		if AsHTTPError(err).Status == 429 {
			w.Header().Set("Retry-After", "2")
		}
		writeError(w, r, err)
		return
	}
	defer releaseRequest()
	ctx = context.WithValue(ctx, dopBatchParallelKey{}, dopBatchParallelMode{provision: !e.options.DisableParallelProvision})
	r = r.WithContext(ctx)
	// Planning and preflight use the same connection budget as item execution.
	release, err := e.acquireSlot(ctx)
	if err != nil {
		api.writeConcurrentError(w, r, err, parent)
		return
	}
	plan, err := func() (dopBatchPlan, error) {
		defer release()
		if operation == "users/provision" {
			var err error
			r, err = api.provisionPreflight(r)
			if err != nil {
				return dopBatchPlan{}, err
			}
		}
		return api.planConcurrent(ctx, operation, items)
	}()
	if err != nil {
		api.writeConcurrentError(w, r, err, parent)
		return
	}
	results, err := e.run(r.Context(), plan.groups, len(items), workers, func(ctx context.Context, index int) dopBatchResult {
		return api.executeConcurrentItem(r.WithContext(ctx), user, operation, key, items[index], plan.keys[index])
	})
	if err != nil {
		api.writeConcurrentError(w, r, err, parent)
		return
	}
	succeeded, pending, failed := 0, 0, 0
	for i := range results {
		results[i].ClientRef = items[i].ClientRef
		switch results[i].State {
		case "succeeded":
			succeeded++
		case "pending":
			pending++
		default:
			failed++
		}
	}
	status := http.StatusOK
	if failed+pending > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"data": results, "total": len(results), "succeeded": succeeded, "pending": pending, "failed": failed,
		"execution": map[string]any{"requested_concurrency": workers, "concurrency": workers, "mode": "concurrent", "global_limit": e.capacity,
			"parallel_provision": operation == "users/provision" && !e.options.DisableParallelProvision}})
}

func (api *dopBatchAPI) writeConcurrentError(w http.ResponseWriter, r *http.Request, err error, parent context.Context) {
	if parent.Err() != nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		writeError(w, r, NewHTTPError(503, "batch_interrupted", "Batch interrupted; retry with the same idempotency key and client refs"))
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, r, NewHTTPError(504, "batch_deadline_exceeded", "Batch interrupted; reconcile by retrying with the same idempotency key and client refs"))
		return
	}
	writeError(w, r, dopBatchSafeError(err))
}

func (api *dopBatchAPI) executeConcurrentItem(r *http.Request, user AdminUser, operation, key string, item dopBatchItem, keys []string) dopBatchResult {
	failed := func(err error) dopBatchResult {
		code, body := dopBatchErrorBody(err)
		return dopBatchResult{ClientRef: item.ClientRef, Status: code, State: "failed", Body: body}
	}
	release, err := api.executor.leases.acquire(r.Context(), keys)
	if err != nil {
		return failed(err)
	}
	defer release()
	for attempt := 0; ; attempt++ {
		releaseSlot, err := api.executor.acquireSlot(r.Context())
		if err != nil {
			return failed(err)
		}
		var result dopBatchResult
		var retry bool
		func() { defer releaseSlot(); result, retry = api.executeItemAttempt(r, user, operation, key, item) }()
		if !retry || attempt >= 2 {
			return result
		}
		timer := time.NewTimer(time.Duration(20+rand.IntN(80)) * time.Millisecond)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return failed(r.Context().Err())
		}
	}
}
