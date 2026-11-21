// Package index scans manifest-referenced media into a persistent index with
// per-segment digests and resumable progress state.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediascan/internal/manifest"
	"mediascan/internal/model"
)

// Version is bumped whenever the on-disk index layout changes. Checkpoints
// bind to it so stale state is never silently reused.
const Version = 1

// Entry is one scanned manifest segment.
type Entry struct {
	Ordinal   int            `json:"ordinal"`
	StreamID  string         `json:"stream_id"`
	Name      string         `json:"name"`
	URI       string         `json:"uri"`
	ByteStart int64          `json:"byte_start"`
	ByteEnd   int64          `json:"byte_end"`
	Size      int64          `json:"size"`
	SHA256    string         `json:"sha256"`
	Start     model.Rational `json:"start"`
	Duration  model.Rational `json:"duration"`
	Present   bool           `json:"present"`
	Truncated bool           `json:"truncated,omitempty"`
}

// End returns the segment's exclusive end time.
func (e Entry) End() (model.Rational, error) { return e.Start.Add(e.Duration) }

// Index is the persisted result of a scan.
type Index struct {
	Version   int            `json:"version"`
	Manifest  string         `json:"manifest"`
	Format    string         `json:"format"`
	Timescale uint32         `json:"timescale"`
	Duration  model.Rational `json:"duration"`
	InputSHA  string         `json:"input_sha256"`
	Entries   []Entry        `json:"entries"`
	Missing   []string       `json:"missing,omitempty"`
	FirstMiss string         `json:"first_missing,omitempty"`
	Faults    []model.Fault  `json:"faults,omitempty"`
	Scanned   int            `json:"scanned"`
	Skipped   int            `json:"skipped"`
}

// Paths describes the files the index subsystem owns inside a work dir.
type Paths struct {
	Work     string
	Index    string
	Progress string
}

// NewPaths returns the canonical file layout for a work directory.
func NewPaths(work string) Paths {
	return Paths{
		Work:     work,
		Index:    filepath.Join(work, "index.json"),
		Progress: filepath.Join(work, "progress.json"),
	}
}

// WriteJSON writes v atomically through a temp file and rename.
func WriteJSON(path string, v any) error {
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadIndex loads an index file.
func ReadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("index: corrupt %s: %w", path, err)
	}
	return &idx, nil
}

// HashBytes returns the lowercase hex sha256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashFile streams a file through sha256 without buffering it whole.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashReader streams r through sha256.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashTimeline returns a stable digest over the manifest's normalized
// timeline: names, byte ranges and exact times, in order.
func HashTimeline(tl *manifest.Timeline) string {
	h := sha256.New()
	fmt.Fprintf(h, "format=%s\ntimescale=%d\nduration=%s\n", tl.Format, tl.Timescale, tl.Duration)
	for _, st := range tl.Streams {
		fmt.Fprintf(h, "stream=%s|%s|%d|%s\n", st.ID, st.Kind, st.Timescale, st.InitURI)
		for _, seg := range st.Segments {
			fmt.Fprintf(h, "seg=%s|%s|%d|%d|%s|%s\n",
				seg.Name, seg.URI, seg.ByteStart, seg.ByteEnd, seg.Start, seg.Duration)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
