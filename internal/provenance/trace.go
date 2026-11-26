// Package provenance records, for every output segment, the input digest,
// sample ranges, repair decisions and output hashes, so an independent
// verifier can re-check the provenance chain.
package provenance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Segment decisions recorded in the trace.
const (
	Accepted    = "accepted"
	Duplicate   = "duplicate"
	Quarantined = "quarantined"
	Lost        = "lost"
)

// TrackTrace records one track's sample range and payload hash within a
// segment.
type TrackTrace struct {
	TrackID    uint32 `json:"track_id"`
	Samples    int    `json:"samples"`
	FirstDTS   int64  `json:"first_dts"`
	LastDTS    int64  `json:"last_dts"`
	PayloadSHA string `json:"payload_sha256"`
}

// SegmentTrace records the provenance of one input segment position.
type SegmentTrace struct {
	Seq       uint32       `json:"seq"`
	File      string       `json:"file,omitempty"`
	Decision  string       `json:"decision"`
	InputSHA  string       `json:"input_sha256,omitempty"`
	Repairs   []string     `json:"repairs,omitempty"`
	Tracks    []TrackTrace `json:"tracks,omitempty"`
	LatencyMs float64      `json:"latency_ms,omitempty"`
}

// Trace stores provenance as one JSONL file per chunk so a redone chunk is
// replaced atomically instead of appended twice.
type Trace struct {
	dir string
}

// Open creates (or reuses) the provenance directory.
func Open(dir string) (*Trace, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Trace{dir: dir}, nil
}

func (t *Trace) chunkPath(chunk int) string {
	return filepath.Join(t.dir, fmt.Sprintf("chunk_%04d.jsonl", chunk))
}

// WriteChunk atomically replaces the provenance of one chunk.
func (t *Trace) WriteChunk(chunk int, recs []SegmentTrace) error {
	tmp, err := os.CreateTemp(t.dir, ".prov-*")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), t.chunkPath(chunk))
}

// Load reads all chunk files in chunk order.
func (t *Trace) Load() ([]SegmentTrace, error) {
	names, err := filepath.Glob(filepath.Join(t.dir, "chunk_*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []SegmentTrace
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1<<20), 1<<20)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if line == "" {
				continue
			}
			var r SegmentTrace
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out = append(out, r)
		}
		f.Close()
		if err := s.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
