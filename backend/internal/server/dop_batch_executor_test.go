package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dopTestExecutor(t *testing.T, capacity int) *dopBatchExecutor {
	t.Helper()
	options, err := (DOPBatchOptions{}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	return &dopBatchExecutor{options: options, capacity: capacity, slots: make(chan struct{}, capacity), active: make(map[uint64]context.CancelFunc), drained: make(chan struct{})}
}

func TestDOPExecutorGlobalLimitAndOrderedResults(t *testing.T) {
	e := dopTestExecutor(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	barrier := make(chan struct{})
	entered := make(chan struct{}, 3)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			groups := make([][]int, 20)
			for i := range groups {
				groups[i] = []int{i}
			}
			results, err := e.run(ctx, groups, 20, 4, func(ctx context.Context, index int) dopBatchResult {
				release, err := e.acquireSlot(ctx)
				if err != nil {
					return dopBatchResult{}
				}
				defer release()
				current := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); current > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, current) {
						break
					}
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-barrier:
				case <-ctx.Done():
				}
				body, _ := json.Marshal(index)
				return dopBatchResult{State: "succeeded", Body: body}
			})
			if err != nil || len(results) != 20 {
				t.Errorf("executor failed: %v", err)
				return
			}
			for i, result := range results {
				if string(result.Body) != fmt.Sprint(i) {
					t.Errorf("result index %d changed", i)
				}
			}
		})
	}
	for range 3 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("workers did not execute concurrently")
		}
	}
	if active.Load() != 3 {
		t.Fatal("global capacity was not used")
	}
	close(barrier)
	wg.Wait()
	if peak.Load() != 3 || active.Load() != 0 || len(e.slots) != 0 {
		t.Fatal("global permit leak or limit violation")
	}
}

func TestDOPExecutorDependencyOrderAndPanicIsolation(t *testing.T) {
	e := dopTestExecutor(t, 4)
	keys := [][]string{{"x"}, {"y"}, {"x"}, {"z", "x"}, {"z"}}
	groups := groupDOPBatchDependencies(keys)
	var mu sync.Mutex
	var order []int
	results, err := e.run(context.Background(), groups, len(keys), 4, func(_ context.Context, index int) dopBatchResult {
		mu.Lock()
		order = append(order, index)
		mu.Unlock()
		if index == 1 {
			panic("synthetic-secret-must-not-appear")
		}
		return dopBatchResult{State: "succeeded"}
	})
	if err != nil || len(results) != 5 || results[1].Status != 500 || strings.Contains(string(results[1].Body), "synthetic-secret") {
		t.Fatal("panic escaped or lost item results")
	}
	var related []int
	for _, i := range order {
		if i != 1 {
			related = append(related, i)
		}
	}
	if fmt.Sprint(related) != "[0 2 3 4]" {
		t.Fatalf("dependency order changed: %v", related)
	}
}

func TestDOPExecutorCancellationAndAdmissionShutdown(t *testing.T) {
	e := dopTestExecutor(t, 4)
	first, releaseFirst, err := e.admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, releaseSecond, err := e.admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.admit(context.Background()); AsHTTPError(err).Status != 429 {
		t.Fatal("excess requests were admitted")
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := e.run(first, [][]int{{0}}, 1, 1, func(ctx context.Context, _ int) dopBatchResult { close(started); <-ctx.Done(); return dopBatchResult{} })
		finished <- err
	}()
	<-started
	stop, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(e.shutdown(stop), context.Canceled) {
		t.Fatal("shutdown ignored its deadline")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("run did not cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker leaked")
	}
	if second.Err() == nil {
		t.Fatal("other active request was not canceled")
	}
	releaseFirst()
	releaseSecond()
	if err := e.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.admit(context.Background()); AsHTTPError(err).Status != 503 {
		t.Fatal("shutdown accepted a request")
	}
}

func TestDOPResourceLeasesCancelAndRelease(t *testing.T) {
	var leases dopBatchResourceLeases
	release, err := leases.acquire(context.Background(), []string{"same", "same"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := leases.acquire(ctx, []string{"same"}); !errors.Is(err, context.Canceled) {
		t.Fatal("blocked lease did not cancel")
	}
	release()
	if len(leases.entries) != 0 {
		t.Fatal("resource lease entries leaked")
	}
}

func TestDOPExecutorTwentyThousand(t *testing.T) {
	e := dopTestExecutor(t, 8)
	groups := make([][]int, dopBatchLimit)
	for i := range groups {
		groups[i] = []int{i}
	}
	var processed atomic.Int32
	results, err := e.run(context.Background(), groups, len(groups), 8, func(_ context.Context, index int) dopBatchResult {
		processed.Add(1)
		return dopBatchResult{ClientRef: fmt.Sprint(index), State: "succeeded"}
	})
	if err != nil || len(results) != dopBatchLimit || int(processed.Load()) != dopBatchLimit {
		t.Fatal("maximum-size pool lost work")
	}
	for i, result := range results {
		if result.ClientRef != fmt.Sprint(i) {
			t.Fatal("maximum-size results lost ordering")
		}
	}
}
