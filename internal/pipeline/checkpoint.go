package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/adapter"
	"mediaroute/internal/fault"
	"mediaroute/internal/manifest"
	"mediaroute/internal/reorder"
)

// State is the full checkpointable engine state: packet windows, event
// watermark (items processed + delivered events) and hash state.
type State struct {
	ItemsProcessed int                            `json:"items_processed"`
	Clocks         map[string][4]int64            `json:"clocks"`
	Windows        map[string]reorder.WindowState `json:"windows"`
	Recovery       reorder.Recovery               `json:"recovery"`
	Delivered      []adapter.Event                `json:"delivered,omitempty"`
	Isolated       map[string]bool                `json:"isolated,omitempty"`
	Errors         []*fault.Error                 `json:"errors,omitempty"`
	HashState      string                         `json:"hash_state"`
}

// Snapshot captures the engine state at the given item watermark.
func (e *Engine) Snapshot(items int) *State {
	s := &State{
		ItemsProcessed: items,
		Clocks:         e.rtp.Snapshot(),
		Windows:        make(map[string]reorder.WindowState, len(e.windows)),
		Recovery:       *e.rec,
		Delivered:      append([]adapter.Event(nil), e.delivered...),
		Isolated:       map[string]bool{},
		Errors:         append([]*fault.Error(nil), e.Errs.Errors...),
		HashState:      manifest.HashSegment(manifest.Segment{}, e.delivered),
	}
	for track, w := range e.windows {
		s.Windows[track] = w.Snapshot()
	}
	for k, v := range e.isolated {
		s.Isolated[k] = v
	}
	return s
}

// Restore reinstates a checkpointed state into the engine.
func (e *Engine) Restore(s *State) {
	e.rtp.Restore(s.Clocks)
	e.windows = make(map[string]*reorder.Window, len(s.Windows))
	for track, ws := range s.Windows {
		w := reorder.NewWindow(e.windowSz)
		w.Restore(ws)
		e.windows[track] = w
	}
	rec := s.Recovery
	e.rec = &rec
	e.delivered = append([]adapter.Event(nil), s.Delivered...)
	e.isolated = map[string]bool{}
	for k, v := range s.Isolated {
		e.isolated[k] = v
	}
	e.Errs = &fault.List{Errors: append([]*fault.Error(nil), s.Errors...)}
}

// checkpointFile is the on-disk form: payload plus integrity checksum.
type checkpointFile struct {
	Items    int             `json:"items"`
	Checksum string          `json:"checksum"`
	Payload  json.RawMessage `json:"payload"`
}

// Checkpointer saves rotating checkpoint generations so a corrupted
// current checkpoint falls back to the previous complete one.
type Checkpointer struct {
	dir      string
	interval int
}

// NewCheckpointer saves every interval processed items into dir.
func NewCheckpointer(dir string, interval int) *Checkpointer {
	return &Checkpointer{dir: dir, interval: interval}
}

// MaybeSave saves a snapshot when the interval boundary is crossed.
func (c *Checkpointer) MaybeSave(eng *Engine, items int) error {
	if c == nil || c.interval <= 0 || items%c.interval != 0 {
		return nil
	}
	return c.Save(eng.Snapshot(items))
}

// Save rotates generations and writes the checkpoint atomically.
func (c *Checkpointer) Save(s *State) error {
	payload, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	sum := sha256.Sum256(payload)
	data, err := json.Marshal(checkpointFile{
		Items: s.ItemsProcessed, Checksum: hex.EncodeToString(sum[:]), Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("marshal checkpoint file: %w", err)
	}
	cur := filepath.Join(c.dir, "checkpoint.json")
	prev := filepath.Join(c.dir, "checkpoint.prev.json")
	if _, err := os.Stat(cur); err == nil {
		if err := os.Rename(cur, prev); err != nil {
			return fmt.Errorf("rotate checkpoint: %w", err)
		}
	}
	tmp := cur + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return os.Rename(tmp, cur)
}

// Recover loads the newest valid checkpoint. A corrupted current
// generation falls back to the previous one; the returned bool reports
// that fallback. Both generations corrupt is a checkpoint fault.
func (c *Checkpointer) Recover() (*State, bool, error) {
	for i, name := range []string{"checkpoint.json", "checkpoint.prev.json"} {
		st, err := c.load(filepath.Join(c.dir, name))
		if err == nil {
			return st, i == 1, nil
		}
	}
	return nil, false, fault.New(fault.CatCheckpoint,
		"no valid checkpoint generation available")
}

func (c *Checkpointer) load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf checkpointFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fault.New(fault.CatCheckpoint, "checkpoint not parseable")
	}
	sum := sha256.Sum256(cf.Payload)
	if hex.EncodeToString(sum[:]) != cf.Checksum {
		return nil, fault.New(fault.CatCheckpoint, "checkpoint checksum mismatch")
	}
	var st State
	if err := json.Unmarshal(cf.Payload, &st); err != nil {
		return nil, fault.New(fault.CatCheckpoint, "checkpoint payload invalid")
	}
	return &st, nil
}
