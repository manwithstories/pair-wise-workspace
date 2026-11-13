package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"example.com/gkpack/internal/manifest"
)

// Block is one deduplicated chunk in the local content-addressed store.
type Block struct {
	Hash string `json:"hash"`
	Size int    `json:"size"`
	Refs int    `json:"refs"`
	Path string `json:"-"`
}

// Store is a small disk-backed content-addressed block store. Every blob is
// written to a sibling temporary file and renamed, so interrupted writes never
// expose a partial block.
type Store struct {
	dir    string
	blocks map[string]*Block
	mu     sync.Mutex
}

// Open opens or creates a store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, blocks: make(map[string]*Block)}
	if err := s.loadRefs(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewTemp creates a temporary store and its cleanup function.
func NewTemp(parent string) (*Store, func(), error) {
	dir, err := os.MkdirTemp(parent, ".gkpack-store-*")
	if err != nil {
		return nil, nil, err
	}
	s, err := Open(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	return s, func() { _ = os.RemoveAll(dir) }, nil
}

// Dir returns the store root directory.
func (s *Store) Dir() string { return s.dir }

// Put stores data under its SHA-256 and increments its reference count.
func (s *Store) Put(data []byte) (Block, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.blocks[hash]; ok {
		b.Refs++
		return *b, nil
	}
	path := s.pathFor(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Block{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		return Block{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return Block{}, err
	}
	if err := tmp.Close(); err != nil {
		return Block{}, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Block{}, err
	}
	ok = true
	b := &Block{Hash: hash, Size: len(data), Refs: 1, Path: path}
	s.blocks[hash] = b
	return *b, nil
}

// AddRef increments the reference count of an existing hash.
func (s *Store) AddRef(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blocks[hash]
	if !ok {
		return fmt.Errorf("block %s not found", hash)
	}
	b.Refs++
	return nil
}

// Get returns a block's raw bytes after checking its content address.
func (s *Store) Get(hash string) ([]byte, error) {
	s.mu.Lock()
	b, ok := s.blocks[hash]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("block %s not found", hash)
	}
	data, err := os.ReadFile(b.Path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("block %s failed hash check", hash)
	}
	return data, nil
}

// Entries returns a stable, hash-sorted snapshot.
func (s *Store) Entries() []Block {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Block, 0, len(s.blocks))
	for _, b := range s.blocks {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// Refs returns the reference count for hash.
func (s *Store) Refs(hash string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.blocks[hash]; ok {
		return b.Refs
	}
	return 0
}

// Reconcile replaces reference counts from the supplied manifests.
func (s *Store) Reconcile(manifests ...manifest.Manifest) {
	refs := make(map[string]int)
	for _, m := range manifests {
		for _, f := range m.Files {
			for _, b := range f.Blocks {
				refs[b.Hash]++
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, b := range s.blocks {
		b.Refs = refs[hash]
	}
}

// GC removes blocks not in keep and returns the number removed.
func (s *Store) GC(keep map[string]bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for hash, b := range s.blocks {
		if keep[hash] {
			continue
		}
		if err := os.Remove(b.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		delete(s.blocks, hash)
		removed++
	}
	if err := s.saveRefsLocked(); err != nil {
		return removed, err
	}
	return removed, nil
}

// GCManifests keeps only blocks referenced by the supplied manifests.
func (s *Store) GCManifests(manifests ...manifest.Manifest) (int, error) {
	keep := make(map[string]bool)
	for _, m := range manifests {
		for hash := range m.BlockSet() {
			keep[hash] = true
		}
	}
	return s.GC(keep)
}

// Close persists reference counts and releases the handle-like store state.
func (s *Store) Close() error {
	return s.Save()
}

// Save persists reference counts.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveRefsLocked()
}

func (s *Store) pathFor(hash string) string {
	return filepath.Join(s.dir, "blocks", hash[:2], hash)
}

func (s *Store) refsPath() string {
	return filepath.Join(s.dir, "refs.json")
}

func (s *Store) saveRefsLocked() error {
	list := make([]Block, 0, len(s.blocks))
	for _, b := range s.blocks {
		list = append(list, Block{Hash: b.Hash, Size: b.Size, Refs: b.Refs})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Hash < list[j].Hash })
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := s.refsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.refsPath())
}

func (s *Store) loadRefs() error {
	data, err := os.ReadFile(s.refsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return s.discover()
	}
	if err != nil {
		return err
	}
	var list []Block
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, b := range list {
		b.Path = s.pathFor(b.Hash)
		bb := b
		s.blocks[b.Hash] = &bb
	}
	return s.discoverMissing()
}

func (s *Store) discover() error {
	return s.walkBlocks(func(path, name string) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		s.blocks[name] = &Block{Hash: name, Size: int(info.Size()), Refs: 0, Path: path}
		return nil
	})
}

func (s *Store) discoverMissing() error {
	return s.walkBlocks(func(path, name string) error {
		if _, ok := s.blocks[name]; ok {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		s.blocks[name] = &Block{Hash: name, Size: int(info.Size()), Refs: 0, Path: path}
		return nil
	})
}

func (s *Store) walkBlocks(fn func(path, name string) error) error {
	root := filepath.Join(s.dir, "blocks")
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || len(name) != 64 {
			return nil
		}
		return fn(path, name)
	})
}
