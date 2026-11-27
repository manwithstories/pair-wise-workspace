// Package audit produces the atomic manifest that records what a run did and
// verifies it afterwards.
//
// The manifest is written to a temporary file and renamed into place, so a
// crash never leaves a half-written manifest. It records, per segment, the
// ordered packet hashes, the byte and packet counts, the clock decisions that
// caused a cut and the origin of the first packet. Verify recomputes every
// hash from the segment hashes recorded in the manifest and, when the output
// files are still present, from the bytes on disk, so a re-run can be compared
// segment by segment.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"avpipe/internal/media"
)

// ManifestVersion is bumped whenever the manifest schema changes
// incompatibly.
const ManifestVersion = 1

// SegmentRecord is one audited segment.
type SegmentRecord struct {
	Index       int             `json:"index"`
	Start       media.Rational  `json:"start"`
	End         media.Rational  `json:"end"`
	Packets     int             `json:"packets"`
	Bytes       int64           `json:"bytes"`
	FirstKey    bool            `json:"first_key"`
	Hash        string          `json:"hash"`
	Reasons     []string        `json:"reasons,omitempty"`
	FirstOrigin media.Origin    `json:"first_origin"`
	Tracks      []string        `json:"tracks"`
}

// TrackRecord records a mapped track and its display decision.
type TrackRecord struct {
	ID         string              `json:"id"`
	Source     int                 `json:"source"`
	Kind       media.TrackKind     `json:"kind"`
	Codec      string              `json:"codec"`
	TimeBase   media.Rational      `json:"time_base"`
	Display    media.DisplayInfo   `json:"display"`
}

// Manifest is the audit record of one run.
type Manifest struct {
	Version     int             `json:"version"`
	Seed        int64           `json:"seed"`
	Workers     int             `json:"workers"`
	Queue       int             `json:"queue"`
	SegmentSecs media.Rational  `json:"segment_seconds"`
	CreatedAt   string          `json:"created_at"`
	Inputs      []string        `json:"inputs"`
	Tracks      []TrackRecord   `json:"tracks"`
	Segments    []SegmentRecord `json:"segments"`
	// Totals
	Packets          int     `json:"packets"`
	Bytes            int64   `json:"bytes"`
	WrapRecovered    int     `json:"wrap_recovered"`
	VFRFrames        int     `json:"vfr_frames"`
	CorruptRejected  int     `json:"corrupt_rejected"`
	Quarantined      int     `json:"quarantined"`
	QueuePeak        int     `json:"queue_peak"`
	Canceled         bool    `json:"canceled"`
	// Digest of the whole ordered output, used for replay equality.
	OutputDigest string `json:"output_digest"`
	// SegmentDigests maps segment index to hash for cross-run comparison.
	SegmentDigests map[string]string `json:"segment_digests"`
}

// Writer accumulates a manifest and commits it atomically.
type Writer struct {
	path string
	m    Manifest
	done bool
}

// NewWriter starts a manifest for path. The parent directory must exist.
func NewWriter(path string, seed int64) (*Writer, error) {
	if path == "" {
		return nil, media.Errorf(media.KindUsage, media.Origin{}, "audit.NewWriter", "manifest path is required")
	}
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, media.Errorf(media.KindUsage, media.Origin{File: path}, "audit.NewWriter",
			"manifest directory %q does not exist", dir)
	}
	return &Writer{
		path: path,
		m: Manifest{
			Version: ManifestVersion, Seed: seed,
			SegmentDigests: map[string]string{},
			Tracks:         []TrackRecord{}, Segments: []SegmentRecord{},
		},
	}, nil
}

// Manifest returns the in-progress manifest for filling in totals.
func (w *Writer) Manifest() *Manifest { return &w.m }

// AddTrack records a mapped track.
func (w *Writer) AddTrack(t media.Track) {
	w.m.Tracks = append(w.m.Tracks, TrackRecord{
		ID: t.ID, Source: t.Source, Kind: t.Kind, Codec: t.Codec,
		TimeBase: t.TimeBase, Display: t.Display,
	})
}

// AddInput records an input file path.
func (w *Writer) AddInput(path string) { w.m.Inputs = append(w.m.Inputs, path) }

// AddSegment records one segment and indexes its digest.
func (w *Writer) AddSegment(s SegmentRecord) {
	w.m.Segments = append(w.m.Segments, s)
	w.m.SegmentDigests[fmt.Sprintf("%d", s.Index)] = s.Hash
}

// Commit computes the output digest, stamps the time and writes the manifest
// atomically. The output digest covers the ordered segment hashes, so two runs
// with the same segments produce the same digest regardless of wall clock.
func (w *Writer) Commit() error {
	if w.done {
		return media.Errorf(media.KindInternal, media.Origin{File: w.path}, "audit.Commit", "already committed")
	}
	w.m.OutputDigest = outputDigest(w.m.Segments)
	if w.m.CreatedAt == "" {
		w.m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	sort.SliceStable(w.m.Segments, func(i, j int) bool { return w.m.Segments[i].Index < w.m.Segments[j].Index })
	data, err := json.MarshalIndent(&w.m, "", "  ")
	if err != nil {
		return media.New(media.KindInternal, media.Origin{File: w.path}, "audit.Commit", "encoding manifest failed", err)
	}
	if err := writeAtomic(w.path, data, 0o644); err != nil {
		return err
	}
	w.done = true
	return nil
}

// outputDigest hashes the ordered segment hashes, which themselves hash the
// ordered packet hashes. Any change to packet content or order changes it.
func outputDigest(segs []SegmentRecord) string {
	ordered := make([]SegmentRecord, len(segs))
	copy(ordered, segs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	h := sha256.New()
	for _, s := range ordered {
		_, _ = fmt.Fprintf(h, "%d:%s:%d:%d\n", s.Index, s.Hash, s.Packets, s.Bytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeAtomic writes data to a temp file in the destination directory and
// renames it into place, so readers never observe a partial manifest.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "creating temp file failed", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "writing temp file failed", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "syncing temp file failed", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "closing temp file failed", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "chmod failed", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return media.New(media.KindInternal, media.Origin{File: path}, "audit.writeAtomic", "renaming failed", err)
	}
	return nil
}
