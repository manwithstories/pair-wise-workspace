package versiondb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"example.com/gkpack/internal/delta"
	"example.com/gkpack/internal/pack"
)

const dbFile = "versions.json"

// Version identifies one immutable archive and its graph metadata.
type Version struct {
	Name    string `json:"name"`
	Branch  string `json:"branch"`
	Parent  string `json:"parent,omitempty"`
	Archive string `json:"archive"`
	Root    string `json:"root"`
}

// Edge records an available bidirectional patch.
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Patch string `json:"patch"`
}

// DB is the persisted version graph.
type DB struct {
	Versions map[string]Version `json:"versions"`
	Patches  []Edge             `json:"patches"`
	dir      string
}

// Hop is one oriented edge in a shortest path.
type Hop struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Patch      string `json:"patch"`
	Reverse    bool   `json:"reverse"`
	BranchFrom string `json:"branch_from"`
	BranchTo   string `json:"branch_to"`
}

// Open loads or creates a version graph at dir.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{Versions: make(map[string]Version), Patches: []Edge{}, dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, dbFile))
	if errors.Is(err, os.ErrNotExist) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, db); err != nil {
		return nil, err
	}
	if db.Versions == nil {
		db.Versions = make(map[string]Version)
	}
	db.dir = dir
	return db, nil
}

// Save atomically rewrites the graph.
func (db *DB) Save() error {
	versions := make([]Version, 0, len(db.Versions))
	for _, v := range db.Versions {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Name < versions[j].Name })
	sort.Slice(db.Patches, func(i, j int) bool {
		if db.Patches[i].From != db.Patches[j].From {
			return db.Patches[i].From < db.Patches[j].From
		}
		return db.Patches[i].To < db.Patches[j].To
	})
	data, err := json.MarshalIndent(struct {
		Versions []Version `json:"versions"`
		Patches  []Edge    `json:"patches"`
	}{versions, db.Patches}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(db.dir, dbFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(db.dir, dbFile))
}

// AddVersion validates the archive root and registers a version.
func (db *DB) AddVersion(v Version) error {
	if v.Name == "" || v.Branch == "" || v.Archive == "" {
		return errors.New("version name, branch, and archive are required")
	}
	if _, exists := db.Versions[v.Name]; exists {
		return fmt.Errorf("version %q already exists", v.Name)
	}
	if v.Parent != "" {
		if _, ok := db.Versions[v.Parent]; !ok {
			return fmt.Errorf("parent version %q does not exist", v.Parent)
		}
	}
	reader, err := pack.Open(v.Archive)
	if err != nil {
		return fmt.Errorf("open version archive: %w", err)
	}
	root := reader.Root()
	_ = reader.Close()
	if v.Root != "" && v.Root != root {
		return fmt.Errorf("archive root %s does not match requested %s", root, v.Root)
	}
	v.Root = root
	db.Versions[v.Name] = v
	return db.Save()
}

// AddPatch registers a bidirectional patch and checks that it matches the two
// registered versions.
func (db *DB) AddPatch(from, to, patchPath string) error {
	fv, ok := db.Versions[from]
	if !ok {
		return fmt.Errorf("version %q does not exist", from)
	}
	tv, ok := db.Versions[to]
	if !ok {
		return fmt.Errorf("version %q does not exist", to)
	}
	reader, err := delta.Open(patchPath)
	if err != nil {
		return fmt.Errorf("open patch: %w", err)
	}
	h := reader.Header()
	_ = reader.Close()
	if h.BaseRoot != fv.Root || h.TargetRoot != tv.Root {
		return fmt.Errorf("patch roots (%s -> %s) do not match versions (%s -> %s)", h.BaseRoot, h.TargetRoot, fv.Root, tv.Root)
	}
	for i := range db.Patches {
		e := &db.Patches[i]
		if (e.From == from && e.To == to) || (e.From == to && e.To == from) {
			e.Patch = patchPath
			return db.Save()
		}
	}
	db.Patches = append(db.Patches, Edge{From: from, To: to, Patch: patchPath})
	return db.Save()
}

// Path returns the shortest patch path. Edges are bidirectional because every
// patch carries a reverse direction.
func (db *DB) Path(from, to string) ([]Hop, error) {
	if _, ok := db.Versions[from]; !ok {
		return nil, fmt.Errorf("version %q does not exist", from)
	}
	if _, ok := db.Versions[to]; !ok {
		return nil, fmt.Errorf("version %q does not exist", to)
	}
	if from == to {
		return []Hop{}, nil
	}
	type neighbor struct {
		name    string
		patch   string
		reverse bool
	}
	adj := make(map[string][]neighbor)
	for _, e := range db.Patches {
		adj[e.From] = append(adj[e.From], neighbor{name: e.To, patch: e.Patch, reverse: false})
		adj[e.To] = append(adj[e.To], neighbor{name: e.From, patch: e.Patch, reverse: true})
	}
	for name := range adj {
		sort.Slice(adj[name], func(i, j int) bool {
			if adj[name][i].name != adj[name][j].name {
				return adj[name][i].name < adj[name][j].name
			}
			return adj[name][i].patch < adj[name][j].patch
		})
	}
	queue := []string{from}
	prev := map[string]Hop{}
	seen := map[string]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adj[current] {
			if seen[next.name] {
				continue
			}
			seen[next.name] = true
			prev[next.name] = Hop{
				From:       current,
				To:         next.name,
				Patch:      next.patch,
				Reverse:    next.reverse,
				BranchFrom: db.Versions[current].Branch,
				BranchTo:   db.Versions[next.name].Branch,
			}
			if next.name == to {
				return reconstruct(prev, from, to), nil
			}
			queue = append(queue, next.name)
		}
	}
	return nil, fmt.Errorf("version %q is unreachable from %q", to, from)
}

func reconstruct(prev map[string]Hop, from, to string) []Hop {
	var reversed []Hop
	for current := to; current != from; {
		hop, ok := prev[current]
		if !ok {
			return nil
		}
		reversed = append(reversed, hop)
		current = hop.From
	}
	out := make([]Hop, len(reversed))
	for i := range reversed {
		out[len(reversed)-1-i] = reversed[i]
	}
	return out
}

// VersionsSorted returns graph entries by name.
func (db *DB) VersionsSorted() []Version {
	out := make([]Version, 0, len(db.Versions))
	for _, v := range db.Versions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
