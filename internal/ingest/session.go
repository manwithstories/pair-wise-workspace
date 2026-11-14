package ingest

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// SegmentMeta describes one sliced segment in the per-rendition sidecar.
type SegmentMeta struct {
	Seq        int    `json:"seq"`
	File       string `json:"file"`
	DurationMs int    `json:"duration_ms"`
	CRC32      string `json:"crc32"`
}

// GapMeta marks missing sequence slots in the per-rendition sidecar.
type GapMeta struct {
	Seq   int `json:"seq"`
	Count int `json:"count"`
}

// RenditionMeta is the sidecar written by slice and consumed by
// manifest and verify.
type RenditionMeta struct {
	Rendition         string        `json:"rendition"`
	TargetBitrateKbps int           `json:"target_bitrate_kbps"`
	Width             int           `json:"width"`
	Height            int           `json:"height"`
	TargetMs          int           `json:"target_ms"`
	Segments          []SegmentMeta `json:"segments"`
	Gaps              []GapMeta     `json:"gaps"`
	Conflicts         []int64       `json:"conflicts"`
	Duplicates        int           `json:"duplicates"`
	Expired           int           `json:"expired"`
	RebaseEvents      int           `json:"rebase_events"`
}

// ReadMeta loads a per-rendition sidecar.
func ReadMeta(path string) (*RenditionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m RenditionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ingest: parse %s: %w", path, err)
	}
	return &m, nil
}

// CRC32File streams a file and returns its IEEE CRC32.
func CRC32File(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// Session is the per-rendition ingest state machine: it drives the
// reorderer and collects the ordered stream plus gap/conflict stats.
type Session struct {
	Rendition string
	r         *Reorderer
	ordered   []Item
}

func NewSession(rendition string, window int) *Session {
	return &Session{Rendition: rendition, r: NewReorderer(window)}
}

// Push ingests one arrived segment file (computing its CRC lazily upstream).
func (s *Session) Push(it Item) {
	s.ordered = append(s.ordered, s.r.Push(it)...)
}

// Close flushes the reorderer at end of arrivals.
func (s *Session) Close() {
	s.ordered = append(s.ordered, s.r.Flush()...)
}

// Ordered returns the de-duplicated, sequence-ordered items.
func (s *Session) Ordered() []Item { return s.ordered }

// Gaps returns the declared missing sequences.
func (s *Session) Gaps() []int64 { return s.r.Gaps }

// Conflicts returns sequences that arrived with a conflicting CRC.
func (s *Session) Conflicts() []int64 { return s.r.Conflicts }

// Duplicates returns the count of identical retransmissions dropped.
func (s *Session) Duplicates() int { return s.r.Duplicates }

// Expired returns the count of arrivals that fell outside the window.
func (s *Session) Expired() int { return s.r.Expired }

// GapBefore reports how many declared gaps precede each ordered item,
// aligned with the slice returned by Ordered.
func (s *Session) GapBefore() []int {
	out := make([]int, len(s.ordered))
	gaps := s.r.Gaps
	gi := 0
	prev := int64(-1)
	for i, it := range s.ordered {
		n := 0
		for gi < len(gaps) && gaps[gi] < it.Seq {
			if gaps[gi] > prev {
				n++
			}
			gi++
		}
		out[i] = n
		prev = it.Seq
	}
	return out
}

// WriteMetaAtomic serializes the sidecar via a temp file + rename.
func WriteMetaAtomic(path string, m *RenditionMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
