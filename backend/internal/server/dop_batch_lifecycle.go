package server

import (
	"context"
	"net/http"
)

type dopBatchHTTPHandler struct {
	http.Handler
	executor *dopBatchExecutor
}

func (h *dopBatchHTTPHandler) StopAccepting()                     { h.executor.stopAccepting() }
func (h *dopBatchHTTPHandler) Shutdown(ctx context.Context) error { return h.executor.shutdown(ctx) }

func (e *dopBatchExecutor) admit(parent context.Context) (context.Context, func(), error) {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	if e.closing {
		return nil, nil, NewHTTPError(503, "batch_shutting_down", "Concurrent batch execution is shutting down")
	}
	if len(e.active) >= e.options.MaxRequests {
		return nil, nil, NewHTTPError(429, "batch_concurrency_busy", "Concurrent batches are busy; retry with the same idempotency key")
	}
	ctx, cancel := context.WithTimeout(parent, e.options.Timeout)
	e.nextID++
	id := e.nextID
	e.active[id] = cancel
	release := func() {
		cancel()
		e.lifeMu.Lock()
		delete(e.active, id)
		e.closeDrained()
		e.lifeMu.Unlock()
	}
	return ctx, release, nil
}

func (e *dopBatchExecutor) closeDrained() {
	if e.closing && len(e.active) == 0 {
		e.drainOnce.Do(func() { close(e.drained) })
	}
}

func (e *dopBatchExecutor) stopAccepting() {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	e.closing = true
	e.closeDrained()
}

func (e *dopBatchExecutor) shutdown(ctx context.Context) error {
	e.stopAccepting()
	select {
	case <-e.drained:
		return nil
	case <-ctx.Done():
		e.lifeMu.Lock()
		for _, cancel := range e.active {
			cancel()
		}
		e.lifeMu.Unlock()
		return ctx.Err()
	}
}
