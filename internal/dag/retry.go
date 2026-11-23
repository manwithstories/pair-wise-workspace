package dag

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"medpipe/internal/model"
)

// Record is one durable node outcome. Records are keyed by idempotency key,
// so a retry of identical inputs finds the prior record instead of
// appending a duplicate.
type Record struct {
	Key    string   `json:"key"`
	Node   string   `json:"node"`
	Inputs []string `json:"inputs"`
	Digest string   `json:"digest"`
	Detail string   `json:"detail"`
}

type storeFile struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Store is an on-disk idempotency log. It is safe for concurrent use within
// one process and rewrites atomically.
type Store struct {
	mu      sync.Mutex
	path    string
	records map[string]Record
	order   []string
}

// OpenStore loads or creates a store at path.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, model.New(model.KindInput, "dag.OpenStore", "empty store path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, model.Wrap(model.KindInput, "dag.OpenStore", err)
	}
	s := &Store{path: path, records: map[string]Record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, model.Wrap(model.KindInput, "dag.OpenStore", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, model.Wrap(model.KindCorrupt, "dag.OpenStore", err)
	}
	if f.Version != 1 {
		return nil, model.New(model.KindInput, "dag.OpenStore", fmt.Sprintf("unsupported store version %d", f.Version))
	}
	for _, r := range f.Records {
		if _, dup := s.records[r.Key]; dup {
			return nil, model.New(model.KindCorrupt, "dag.OpenStore", fmt.Sprintf("duplicate record key %s", r.Key))
		}
		s.records[r.Key] = r
		s.order = append(s.order, r.Key)
	}
	return s, nil
}

// Lookup returns a stored record by key.
func (s *Store) Lookup(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[key]
	return r, ok
}

// Put records a result. Storing the same key twice is a no-op that keeps the
// original record, which is what makes retries idempotent.
func (s *Store) Put(r Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[r.Key]; ok {
		return existing, false, nil
	}
	s.records[r.Key] = r
	s.order = append(s.order, r.Key)
	if err := s.flushLocked(); err != nil {
		delete(s.records, r.Key)
		s.order = s.order[:len(s.order)-1]
		return Record{}, false, err
	}
	return r, true, nil
}

// Len is the number of stored records.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Keys returns stored keys in insertion order.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

func (s *Store) flushLocked() error {
	recs := make([]Record, 0, len(s.order))
	for _, k := range s.order {
		recs = append(recs, s.records[k])
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Key < recs[j].Key })
	data, err := json.MarshalIndent(storeFile{Version: 1, Records: recs}, "", "  ")
	if err != nil {
		return model.Wrap(model.KindInvariant, "dag.Store.flush", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".dagstore-*")
	if err != nil {
		return model.Wrap(model.KindInput, "dag.Store.flush", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return model.Wrap(model.KindInput, "dag.Store.flush", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return model.Wrap(model.KindInput, "dag.Store.flush", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return model.Wrap(model.KindInput, "dag.Store.flush", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		return model.Wrap(model.KindInput, "dag.Store.flush", err)
	}
	return nil
}
