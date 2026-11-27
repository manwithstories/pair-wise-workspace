// Package dag runs the relay processing graph as idempotent, resumable
// nodes. Each node derives a stable key from its inputs and parameters, so a
// retry never reproduces an artifact; journal.go persists node state
// atomically, letting an interrupted run resume by re-executing only the
// nodes that never finished.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Status values persisted per node.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusPaused    Status = "paused"
)

// Entry is one journaled node state record.
type Entry struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// ErrNotFound means no journal entry exists for a key.
var ErrNotFound = errors.New("dag: journal entry not found")

// Journal is a JSON state file rewritten atomically (temp file + rename) so
// a crash can never leave a half-written state.
type Journal struct {
	path    string
	entries map[string]Entry
}

// OpenJournal loads an existing journal or starts an empty one at dir.
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("dag: create state dir: %w", err)
	}
	j := &Journal{
		path:    filepath.Join(dir, "journal.json"),
		entries: map[string]Entry{},
	}
	raw, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dag: read journal: %w", err)
	}
	var list []Entry
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("dag: corrupt journal: %w", err)
	}
	for _, e := range list {
		j.entries[e.Key] = e
	}
	return j, nil
}

// Get returns the entry for key.
func (j *Journal) Get(key string) (Entry, error) {
	if e, ok := j.entries[key]; ok {
		return e, nil
	}
	return Entry{}, ErrNotFound
}

// IsDone reports whether a node previously succeeded.
func (j *Journal) IsDone(key string) bool {
	e, ok := j.entries[key]
	return ok && e.Status == StatusSucceeded
}

// Put upserts an entry and flushes the whole journal atomically.
func (j *Journal) Put(e Entry) error {
	j.entries[e.Key] = e
	return j.flush()
}

// Snapshot returns entries ordered by key for deterministic rendering.
func (j *Journal) Snapshot() []Entry {
	out := make([]Entry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Key < out[k].Key })
	return out
}

func (j *Journal) flush() error {
	list := j.Snapshot()
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("dag: marshal journal: %w", err)
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("dag: write journal temp: %w", err)
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return fmt.Errorf("dag: commit journal: %w", err)
	}
	return nil
}

// IdempotencyKey derives a stable key from a node name, its input digests
// and its parameters. Identical inputs always yield an identical key, which
// is what makes retries and resume runs produce the same artifacts.
func IdempotencyKey(name string, inputDigests map[string]string, params map[string]string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	writeSorted(h, inputDigests)
	writeSorted(h, params)
	return hex.EncodeToString(h.Sum(nil))
}

func writeSorted(h interface{ Write([]byte) (int, error) }, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0x1f})
		h.Write([]byte(m[k]))
		h.Write([]byte{0x1e})
	}
}
