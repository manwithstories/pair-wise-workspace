// Package replay implements a persistent replay window: a set of used
// license nonces with capacity bounds, window-based eviction and
// concurrency protection. Eviction never drops in-window entries, so a
// duplicate inside the window is always caught.
package replay

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// ErrCacheFull is returned when the cache is full of in-window entries.
var ErrCacheFull = errors.New("replay_cache_full")

type diskState struct {
	Nonces    map[string]time.Time `json:"nonces"`
	LastEpoch int                  `json:"last_epoch"`
}

// Cache tracks used nonces and the highest accepted key epoch.
type Cache struct {
	mu        sync.Mutex
	path      string
	window    time.Duration
	capacity  int
	nonces    map[string]time.Time
	lastEpoch int
}

// Open loads the cache from path; a missing file starts empty.
func Open(path string, window time.Duration, capacity int) (*Cache, error) {
	c := &Cache{
		path:     path,
		window:   window,
		capacity: capacity,
		nonces:   map[string]time.Time{},
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var st diskState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Nonces != nil {
		c.nonces = st.Nonces
	}
	c.lastEpoch = st.LastEpoch
	return c, nil
}

// purgeLocked removes only entries older than the window.
func (c *Cache) purgeLocked(now time.Time) {
	for n, t := range c.nonces {
		if now.Sub(t) > c.window {
			delete(c.nonces, n)
		}
	}
}

// CheckAndAdd reports whether nonce is fresh; fresh nonces are recorded.
// A nonce already present inside the window is a replay and returns false.
func (c *Cache) CheckAndAdd(nonce string, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked(now)
	if _, dup := c.nonces[nonce]; dup {
		return false, nil
	}
	if c.capacity > 0 && len(c.nonces) >= c.capacity {
		return false, ErrCacheFull
	}
	c.nonces[nonce] = now
	return true, nil
}

// LastEpoch returns the highest accepted key epoch so far.
func (c *Cache) LastEpoch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEpoch
}

// UpdateEpoch records epoch if it moves the watermark forward.
func (c *Cache) UpdateEpoch(epoch int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch > c.lastEpoch {
		c.lastEpoch = epoch
	}
}

// Len reports the number of tracked nonces (for tests and report).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked(time.Now())
	return len(c.nonces)
}

// Close persists the cache to disk.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := diskState{Nonces: c.nonces, LastEpoch: c.lastEpoch}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o600)
}
