// Package index defines the timeline index produced by scanning and the
// content-addressed cache that persists it. Cache keys combine the full input
// digest, the scanner version and the manifest digest so that any media or
// manifest change invalidates the entry, and loads verify an embedded
// checksum before the index is trusted.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"mediaroute/internal/clock"
)

// SchemaVersion is the on-disk index schema. Bump it when the representation
// changes in a way that invalidates cached indexes.
const SchemaVersion = 1

// Sample is one entry of a track's sample table with absolute file offsets.
type Sample struct {
	// Number is the 1-based sample number used by stss/stsc.
	Number int64 `json:"number"`
	// DTS and CTS are media ticks; CTS already includes the composition offset.
	DTS int64 `json:"dts"`
	CTS int64 `json:"cts"`
	// Duration is the sample duration in media ticks.
	Duration int64  `json:"duration"`
	Offset   int64  `json:"offset"`
	Size     uint32 `json:"size"`
	// Sync marks a sample declared by stss (or all samples when stss is absent).
	Sync bool `json:"sync"`
	// Readable is false when the sample bytes fall outside the file.
	Readable bool `json:"readable"`
}

// Edit is one presentation edit mapping media ticks onto the timeline.
type Edit struct {
	SegmentDuration clock.Rational `json:"segment_duration"`
	MediaTime       clock.Rational `json:"media_time"`
	Empty           bool           `json:"empty"`
}

// SampleEntry captures the codec-level facts needed by the processing stage.
type SampleEntry struct {
	Type       string `json:"type"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	Layout     string `json:"layout,omitempty"`
}

// Track is the scanned representation of one manifest track.
type Track struct {
	ID        uint32         `json:"id"`
	Kind      string         `json:"kind"`
	Source    string         `json:"source"`
	Timescale uint32         `json:"timescale"`
	Origin    clock.Rational `json:"origin"`
	// EditMediaTicks is subtracted from DTS before adding the origin.
	EditMediaTicks int64          `json:"edit_media_ticks"`
	Duration       clock.Rational `json:"duration"`
	Samples        []Sample       `json:"samples"`
	// Keyframes lists accessible sync sample numbers (1-based, sparse).
	Keyframes []int64     `json:"keyframes"`
	Edits     []Edit      `json:"edits,omitempty"`
	Entry     SampleEntry `json:"entry"`
}

// CorruptRange isolates unreadable media to a time interval so downstream
// scheduling can keep later timestamps untouched.
type CorruptRange struct {
	TrackID     uint32         `json:"track_id"`
	FirstSample int64          `json:"first_sample"`
	LastSample  int64          `json:"last_sample"`
	ByteOffset  int64          `json:"byte_offset"`
	Reason      string         `json:"reason"`
	Start       clock.Rational `json:"start"`
	End         clock.Rational `json:"end"`
}

// Index is the deterministic result of a scan.
type Index struct {
	SchemaVersion  int            `json:"schema_version"`
	ScannerVersion string         `json:"scanner_version"`
	ManifestDigest string         `json:"manifest_digest"`
	InputDigest    string         `json:"input_digest"`
	Tracks         []Track        `json:"tracks"`
	CorruptRanges  []CorruptRange `json:"corrupt_ranges,omitempty"`
	IndexedSamples int            `json:"indexed_samples"`
	Warnings       []string       `json:"warnings,omitempty"`
	// Summary is the digest of the index content; it is excluded from its own
	// computation so repeated scans of identical input agree.
	Summary string `json:"summary,omitempty"`
}

// ComputeSummary hashes the canonical JSON form of the index without Summary.
func (ix *Index) ComputeSummary() (string, error) {
	if ix == nil {
		return "", fmt.Errorf("index: nil")
	}
	cp := *ix
	cp.Summary = ""
	// Ensure empty slices and nil slices hash identically.
	if len(cp.Tracks) == 0 {
		cp.Tracks = []Track{}
	}
	for i := range cp.Tracks {
		if cp.Tracks[i].Samples == nil {
			cp.Tracks[i].Samples = []Sample{}
		}
		if cp.Tracks[i].Keyframes == nil {
			cp.Tracks[i].Keyframes = []int64{}
		}
	}
	if cp.CorruptRanges == nil {
		cp.CorruptRanges = []CorruptRange{}
	}
	data, err := json.Marshal(&cp)
	if err != nil {
		return "", fmt.Errorf("index: marshal for digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Finalize recomputes IndexedSamples and Summary after a scan.
func (ix *Index) Finalize() error {
	n := 0
	for _, t := range ix.Tracks {
		for _, s := range t.Samples {
			if s.Readable {
				n++
			}
		}
	}
	ix.IndexedSamples = n
	sum, err := ix.ComputeSummary()
	if err != nil {
		return err
	}
	ix.Summary = sum
	return nil
}

// TrackByID returns a pointer to the track with the given id.
func (ix *Index) TrackByID(id uint32) *Track {
	for i := range ix.Tracks {
		if ix.Tracks[i].ID == id {
			return &ix.Tracks[i]
		}
	}
	return nil
}

// Key identifies a cached index.
type Key struct {
	InputDigest    string
	ScannerVersion string
	ManifestDigest string
}

// String renders the cache key deterministically.
func (k Key) String() string {
	return fmt.Sprintf("schema=%d|scan=%s|input=%s|manifest=%s", SchemaVersion, k.ScannerVersion, k.InputDigest, k.ManifestDigest)
}
