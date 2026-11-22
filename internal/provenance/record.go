// Package provenance records where every output came from: the source
// manifest digest, the segment range, the time base, the algorithm
// version and the error chain. Records carry no wall-clock fields, so
// re-running the same input with the same params yields the same
// digest.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"mediacore/internal/clock"
)

// Record describes the origin of one output.
type Record struct {
	Output               string   `json:"output"`
	SourceManifestDigest string   `json:"source_manifest_digest"`
	SegmentRange         [2]int   `json:"segment_range"`
	TimeBase             string   `json:"time_base"`
	AlgoVersion          string   `json:"algo_version"`
	ParamsDigest         string   `json:"params_digest"`
	ErrorChain           []string `json:"error_chain"`
}

// Digest returns the deterministic content digest of the record.
func (r Record) Digest() string {
	if r.ErrorChain == nil {
		r.ErrorChain = []string{}
	}
	data, _ := json.Marshal(r) // struct field order is fixed
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Log is an append-only provenance journal (JSON lines).
type Log struct {
	path string
}

// Open returns the journal at path; it is created on first Append.
func Open(path string) *Log { return &Log{path: path} }

// Append adds one record atomically (temp file + rename).
func (l *Log) Append(r Record) error {
	records, err := l.Records()
	if err != nil {
		return err
	}
	records = append(records, r)
	var buf []byte
	for _, rec := range records {
		if rec.ErrorChain == nil {
			rec.ErrorChain = []string{}
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return clock.Wrap(clock.CodeIO, "provenance-append", err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return clock.Wrap(clock.CodeIO, "provenance-append", err)
	}
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return clock.Wrap(clock.CodeIO, "provenance-append", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return clock.Wrap(clock.CodeIO, "provenance-append", err)
	}
	return nil
}

// Records reads the whole journal.
func (l *Log) Records() ([]Record, error) {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, "provenance-read", err)
	}
	var out []Record
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, clock.At(clock.CodeCheckpointCorrupt, "provenance-read", "", -1,
				"bad journal line: %v", err)
		}
		out = append(out, r)
	}
	return out, nil
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}
