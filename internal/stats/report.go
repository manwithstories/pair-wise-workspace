// Package stats aggregates per-mode metrics and writes the JSON report.
package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ModeStats are the headline metrics of one transport mode. Field order
// and names define the terminal/JSON report schema.
type ModeStats struct {
	Transitions int `json:"transitions"`
	Gaps        int `json:"gaps"`
	Frames      int `json:"frames"`
	Duplicates  int `json:"duplicates"`
}

// Report is the top-level report document. Modes are serialized in
// insertion order so the terminal output matches the run order.
type Report struct {
	Modes map[string]*ModeStats `json:"modes"`
	order []string
}

// New returns an empty report.
func New() *Report { return &Report{Modes: make(map[string]*ModeStats)} }

// Add records the stats of one mode.
func (r *Report) Add(mode string, s *ModeStats) {
	if _, ok := r.Modes[mode]; !ok {
		r.order = append(r.order, mode)
	}
	r.Modes[mode] = s
}

// Marshal renders the compact single-line JSON form printed to the
// terminal and stored on disk.
func (r *Report) Marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"modes":{`)
	for i, mode := range r.order {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := json.Marshal(mode)
		if err != nil {
			return nil, fmt.Errorf("stats: marshal report: %w", err)
		}
		st, err := json.Marshal(r.Modes[mode])
		if err != nil {
			return nil, fmt.Errorf("stats: marshal report: %w", err)
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(st)
	}
	b.WriteString("}}")
	return b.Bytes(), nil
}

// WriteFile stores the report at path, creating parent directories.
func (r *Report) WriteFile(path string) error {
	raw, err := r.Marshal()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("stats: create %s: %w", dir, err)
		}
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("stats: write %s: %w", path, err)
	}
	return nil
}
