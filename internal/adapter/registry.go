// Package adapter defines the versioned parser interface and the registry that
// binds manifest references to concrete parsers. Every parser declares a
// format name and a monotonically increasing version; a manifest that pins an
// unknown or unsupported version is rejected with a located error rather than
// being parsed with a best-effort guess.
package adapter

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"mediabridge/internal/fault"
	"mediabridge/internal/media"
)

// Source is the adapter's view of one manifest reference.
type Source struct {
	// AssetID is the manifest-level identity.
	AssetID string
	// Path is the already path-confined, absolute source path.
	Path string
	// Format is the declared adapter format ("mp4", "rtp").
	Format string
	// Version is the declared adapter version. Zero means "latest".
	Version int
	// RootDir is the confinement root the path was resolved against.
	RootDir string
}

// Options tunes parsing limits and behaviour.
type Options struct {
	// MaxPacketsPerTrack caps retained packets per track (delivery limit).
	MaxPacketsPerTrack int64
	// MaxAssetDurationNS caps total asset duration.
	MaxAssetDurationNS int64
	// KeepPayload controls whether encoded bytes are materialised. When false
	// adapters report offsets and sizes only and the kernel re-streams them.
	KeepPayload bool
	// BufferBudget caps total buffered bytes across adapters.
	BufferBudget *Budget
}

// Budget is the shared byte budget interface (implemented by clock.Budget)
// kept abstract here to avoid an import cycle.
type Budget interface {
	Reserve(n int64) error
	Release(n int64)
}

// Adapter is a versioned parser.
type Adapter interface {
	// Format returns the manifest format key (e.g. "mp4").
	Format() string
	// Version returns the parser version. Manifest pins must match exactly
	// unless the manifest requests latest.
	Version() int
	// Name returns a human-readable parser name for diagnostics.
	Name() string
	// Sniff reports whether the adapter claims the given reader. The reader
	// is rewound before and after; Sniff must not consume it.
	Sniff(r io.ReadSeeker) bool
	// Parse translates the source into the shared kernel types.
	Parse(src Source, opts Options) (*media.Asset, error)
}

// Registry stores adapters by format and version.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Adapter)}
}

func key(format string, version int) string { return fmt.Sprintf("%s@v%d", format, version) }

// Register adds an adapter; duplicate (format, version) pairs are rejected.
func (r *Registry) Register(a Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a == nil {
		return fmt.Errorf("adapter: nil adapter")
	}
	if a.Format() == "" || a.Version() <= 0 {
		return fmt.Errorf("adapter: invalid format/version %q v%d", a.Format(), a.Version())
	}
	k := key(a.Format(), a.Version())
	if _, exists := r.byID[k]; exists {
		return fmt.Errorf("adapter: duplicate registration %s", k)
	}
	r.byID[k] = a
	return nil
}

// MustRegister registers or panics; used during init.
func (r *Registry) MustRegister(a Adapter) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Resolve returns the adapter for format/version. Version 0 selects the highest
// registered version. Unknown formats and versions return a located error
// carrying the manifest path so the operator can fix the pin.
func (r *Registry) Resolve(format string, version int, src Source) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	loc := fault.Location{SourcePath: src.Path, AssetID: src.AssetID, Adapter: format}
	if version > 0 {
		if a, ok := r.byID[key(format, version)]; ok {
			return a, nil
		}
		return nil, fault.New(fault.CodeUnknownVersion, loc,
			"no adapter for %s@v%d", format, version).
			WithOp("adapter.Resolve").
			WithRecovery("pin a registered version or omit version for latest")
	}
	// Latest.
	best := -1
	var chosen Adapter
	for _, a := range r.byID {
		if a.Format() == format && a.Version() > best {
			best, chosen = a.Version(), a
		}
	}
	if chosen == nil {
		return nil, fault.New(fault.CodeUnknownVersion, loc,
			"no adapter registered for format %q", format).
			WithOp("adapter.Resolve").
			WithRecovery("add the source format to the manifest registry")
	}
	return chosen, nil
}

// Sniff picks the first adapter (highest version per format) that claims the
// reader. It is used when the manifest omits an explicit format.
func (r *Registry) Sniff(rs io.ReadSeeker) (Adapter, bool) {
	r.mu.RLock()
	adapters := make([]Adapter, 0, len(r.byID))
	for _, a := range r.byID {
		adapters = append(adapters, a)
	}
	r.mu.RUnlock()
	sort.Slice(adapters, func(i, j int) bool {
		if adapters[i].Format() != adapters[j].Format() {
			return adapters[i].Format() < adapters[j].Format()
		}
		return adapters[i].Version() > adapters[j].Version()
	})
	for _, a := range adapters {
		if a.Sniff(rs) {
			return a, true
		}
	}
	return nil, false
}

// Versions lists registered versions for a format, ascending.
func (r *Registry) Versions(format string) []int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []int
	for _, a := range r.byID {
		if a.Format() == format {
			out = append(out, a.Version())
		}
	}
	sort.Ints(out)
	return out
}

// Formats lists registered formats, sorted.
func (r *Registry) Formats() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	for _, a := range r.byID {
		seen[a.Format()] = true
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
