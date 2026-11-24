// Package adapter manages versioned container/codec adapters. Vendor
// format differences stay behind this registry and never enter the core.
package adapter

import (
	"fmt"
	"sort"
	"sync"

	"media-check/internal/model"
)

// Adapter is a versioned format adapter.
type Adapter interface {
	ID() string
	Version() string
}

// Key identifies one registered adapter version.
func Key(id, version string) string { return id + "@" + version }

// Registry holds adapter factories keyed by id@version.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register adds an adapter; re-registering the same key panics, as that
// indicates a build-time wiring bug.
func (r *Registry) Register(a Adapter) {
	k := Key(a.ID(), a.Version())
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.adapters[k]; dup {
		panic("adapter registered twice: " + k)
	}
	r.adapters[k] = a
}

// Get resolves an adapter version. Unknown versions are rejected with a
// structured fault naming the adapter identity, so one bad asset does
// not affect the rest of the batch.
func (r *Registry) Get(id, version string) (Adapter, *model.Fault) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[Key(id, version)]
	if !ok {
		return nil, &model.Fault{
			Code:    model.FaultUnknownAdapter,
			Adapter: Key(id, version),
			Segment: -1,
			Detail:  fmt.Sprintf("no adapter registered for %s", Key(id, version)),
		}
	}
	return a, nil
}

// List returns the registered keys in deterministic order.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for k := range r.adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
