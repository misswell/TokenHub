package server

import (
	"context"
	"fmt"
	"sync"
)

type dopBatchExecutor struct {
	options   DOPBatchOptions
	sqlite    bool
	capacity  int
	slots     chan struct{}
	leases    dopBatchResourceLeases
	lifeMu    sync.Mutex
	closing   bool
	active    map[uint64]context.CancelFunc
	nextID    uint64
	drained   chan struct{}
	drainOnce sync.Once
}

func newDOPBatchExecutor(store *GormStore, options DOPBatchOptions) (*dopBatchExecutor, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	db, err := store.db.DB()
	if err != nil {
		return nil, err
	}
	capacity := options.MaxInFlight
	if maximum := db.Stats().MaxOpenConnections; maximum > 0 {
		capacity = min(capacity, max(0, maximum-*options.ReservedConnections))
	}
	sqlite := store.dbDriver != "postgres"
	if sqlite {
		capacity = 1
	}
	return &dopBatchExecutor{options: options, sqlite: sqlite, capacity: capacity,
		slots: make(chan struct{}, capacity), active: make(map[uint64]context.CancelFunc), drained: make(chan struct{})}, nil
}

func (e *dopBatchExecutor) workers(requested *int) (int, error) {
	if requested == nil || *requested == 1 {
		return 1, nil
	}
	if *requested < 1 || *requested > e.options.MaxConcurrency {
		return 0, NewHTTPError(400, "invalid_batch_concurrency", fmt.Sprintf("concurrency must be between 1 and %d", e.options.MaxConcurrency))
	}
	if e.sqlite {
		return 1, nil
	}
	if e.capacity < 1 {
		return 0, NewHTTPError(503, "batch_parallel_unavailable", "Database capacity is reserved; omit concurrency to use serial processing")
	}
	if *requested > e.capacity {
		return 0, NewHTTPError(400, "invalid_batch_concurrency", fmt.Sprintf("available concurrency is at most %d", e.capacity))
	}
	return *requested, nil
}

func (e *dopBatchExecutor) acquireSlot(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case e.slots <- struct{}{}:
		return func() { <-e.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type dopBatchWork struct{ index, group int }
type dopBatchOutcome struct {
	work   dopBatchWork
	result dopBatchResult
}

// run owns result aggregation. Only one head per dependency group is runnable;
// workers execute one item at a time, with no goroutine per input item.
func (e *dopBatchExecutor) run(ctx context.Context, groups [][]int, count, workers int, execute func(context.Context, int) dopBatchResult) ([]dopBatchResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan dopBatchWork, workers)
	done := make(chan dopBatchOutcome, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := recoverDOPBatchItem(func() dopBatchResult { return execute(ctx, job.index) })
				select {
				case done <- dopBatchOutcome{job, result}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	defer func() { cancel(); close(jobs); wg.Wait() }()
	ready := make([]int, len(groups))
	positions := make([]int, len(groups))
	for i := range ready {
		ready[i] = i
	}
	results := make([]dopBatchResult, count)
	for completed := 0; completed < count; {
		var send chan dopBatchWork
		var next dopBatchWork
		if len(ready) > 0 {
			group := ready[0]
			next = dopBatchWork{groups[group][positions[group]], group}
			send = jobs
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case send <- next:
			ready = ready[1:]
		case outcome := <-done:
			results[outcome.work.index] = outcome.result
			completed++
			group := outcome.work.group
			positions[group]++
			if positions[group] < len(groups[group]) {
				ready = append(ready, group)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func recoverDOPBatchItem(run func() dopBatchResult) (result dopBatchResult) {
	defer func() {
		if recover() != nil {
			result.Status, result.Body = dopBatchErrorBody(NewHTTPError(500, "batch_internal_error", "Batch item could not be completed"))
			result.State = "failed"
		}
	}()
	return run()
}
