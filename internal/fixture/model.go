package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Directory layout inside a batch.
const (
	ManifestHLS  = "index.m3u8"
	ManifestDASH = "manifest.mpd"
	InitName     = "init.mp4"
	ReportName   = "report.json"
)

// Case names a generated sample set.
type Case string

const (
	CaseNormal     Case = "normal"
	CaseTruncated  Case = "truncated"
	CaseCorruptBox Case = "corrupt_box"
	CaseMissingIdx Case = "missing_index"
	CaseTimestamp  Case = "timestamp_jump"
)

// AllCases lists the generated sample directories in a stable order.
var AllCases = []Case{CaseNormal, CaseTruncated, CaseCorruptBox, CaseMissingIdx, CaseTimestamp}

// Options controls generation.
type Options struct {
	Dir          string
	Seed         int64
	Segments     int
	SampleRate   int
	SegmentTicks int64 // canonical ticks per segment
}

// DefaultOptions produces 24 segments of two seconds each.
func DefaultOptions(dir string, seed int64) Options {
	return Options{Dir: dir, Seed: seed, Segments: 24, SampleRate: 48000, SegmentTicks: 180000}
}

// Segment is one generated media segment plus its timeline position.
type Segment struct {
	URI        string
	Sequence   uint64
	StartTicks int64
	Duration   int64
	Bytes      []byte
	Truncated  bool
	Corrupt    bool
	TimeJump   bool
}

// Batch is one generated case directory.
type Batch struct {
	Case       Case
	Dir        string
	SampleRate int
	Segments   []Segment
	HLS        []byte
	DASH       []byte
	InitBytes  []byte
	// KeyframeTicks lists the canonical positions that must be reachable
	// through the index.
	KeyframeTicks []int64
	// HasShippedIndex is false for the missing_index case.
	HasShippedIndex bool
}

// SafeJoin joins a batch-relative URI onto dir and rejects any escape.
func SafeJoin(dir, uri string) (string, error) {
	if uri == "" {
		return "", fmt.Errorf("fixture: empty uri")
	}
	if filepath.IsAbs(uri) {
		return "", fmt.Errorf("fixture: absolute uri %q", uri)
	}
	clean := filepath.Clean(uri)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fixture: uri %q escapes batch", uri)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fixture: uri %q escapes batch", uri)
	}
	return full, nil
}

// writeSafe writes a file atomically inside dir.
func writeSafe(dir, name string, data []byte) error {
	path, err := SafeJoin(dir, name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
