package server

import (
	"context"
	"slices"
	"sort"
	"sync"
)

type dopBatchResourceLease struct {
	gate       chan struct{}
	references int
}
type dopBatchResourceLeases struct {
	mu      sync.Mutex
	entries map[string]*dopBatchResourceLease
}

func (r *dopBatchResourceLeases) acquire(ctx context.Context, keys []string) (func(), error) {
	keys = append([]string(nil), keys...)
	sort.Strings(keys)
	keys = slices.Compact(keys)
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, key := range keys {
		r.mu.Lock()
		if r.entries == nil {
			r.entries = make(map[string]*dopBatchResourceLease)
		}
		entry := r.entries[key]
		if entry == nil {
			entry = &dopBatchResourceLease{gate: make(chan struct{}, 1)}
			r.entries[key] = entry
		}
		entry.references++
		r.mu.Unlock()
		drop := func() {
			r.mu.Lock()
			entry.references--
			if entry.references == 0 {
				delete(r.entries, key)
			}
			r.mu.Unlock()
		}
		select {
		case entry.gate <- struct{}{}:
			releases = append(releases, func() { <-entry.gate; drop() })
		case <-ctx.Done():
			drop()
			release()
			return nil, ctx.Err()
		}
	}
	return release, nil
}
