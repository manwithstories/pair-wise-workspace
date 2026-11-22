package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaslice/media"
)

// ParserVersion is bumped whenever the index schema or parsing semantics
// change; a stale index is always rebuilt.
const ParserVersion = "mediaslice-parser/v2"

// SampleRecord is a read-only view over the columnar sample tables.
type SampleRecord struct {
	Index         int         `json:"i"`
	Offset        int64       `json:"off"`
	Size          int         `json:"size"`
	StartTick     media.Ticks `json:"start"`
	Duration      media.Ticks `json:"dur"`
	Keyframe      bool        `json:"key,omitempty"`
	CorrectedTick media.Ticks `json:"corr,omitempty"`
}

// EditRecord is one retained edit-list entry in track timescale units.
type EditRecord struct {
	SegmentDuration int64 `json:"seg_dur"`
	MediaTime       int64 `json:"media_time"` // may be negative
	Rate            int32 `json:"rate"`
}

// RTPStats summarizes sidecar unwrap/drift processing. Wrap events are
// counted, not retained, to keep long tracks compact.
type RTPStats struct {
	Packets         int         `json:"packets"`
	WrapCount       int         `json:"wraps"`
	TotalCorrection media.Ticks `json:"total_correction"`
	FirstWrapSeq    uint16      `json:"first_wrap_seq,omitempty"`
}

// TrackIndex is the per-track parse result. Sample timing is stored in
// compact columnar form rather than one struct per sample so a multi-hour
// track stays well inside the 256 MiB process budget.
type TrackIndex struct {
	TrackID   uint32       `json:"track_id"`
	Handler   string       `json:"handler"`
	Timescale uint32       `json:"timescale"`
	Edits     []EditRecord `json:"edits,omitempty"`
	// Columnar sample tables (all equal length).
	Offset    []int64       `json:"offsets"`
	Size      []int32       `json:"sizes"`
	StartTick []media.Ticks `json:"start_ticks"`
	Duration  []media.Ticks `json:"durations"`
	Keyframe  []bool        `json:"keyframes"`
	Corrected []media.Ticks `json:"corrected_ticks,omitempty"`
	RTP       *RTPStats     `json:"rtp,omitempty"`
	// Stopped marks a track the scanner abandoned (e.g. unprovable wrap).
	Stopped bool                `json:"stopped,omitempty"`
	Errors  []*media.MediaError `json:"errors,omitempty"`
}

// EndTick returns the media time one tick past the final sample.
func (t *TrackIndex) EndTick() media.Ticks {
	n := len(t.StartTick)
	if n == 0 {
		return 0
	}
	return t.StartTick[n-1] + t.Duration[n-1]
}

// SampleCount returns the number of indexed samples.
func (t *TrackIndex) SampleCount() int { return len(t.StartTick) }

// Sample returns a read-only view of one columnar sample.
func (t *TrackIndex) Sample(i int) SampleRecord {
	var corr media.Ticks
	if len(t.Corrected) > 0 {
		corr = t.Corrected[i]
	}
	return SampleRecord{
		Index: i, Offset: t.Offset[i], Size: int(t.Size[i]),
		StartTick: t.StartTick[i], Duration: t.Duration[i],
		Keyframe: t.Keyframe[i], CorrectedTick: corr,
	}
}

// TrackSummary is a compact digest embedded in the index header.
type TrackSummary struct {
	TrackID      uint32 `json:"track_id"`
	Handler      string `json:"handler"`
	SampleCount  int    `json:"samples"`
	SampleDigest string `json:"digest"`
}

// FileIndex is the complete, verifiable scan result for one MP4 source.
type FileIndex struct {
	ParserVersion string           `json:"parser_version"`
	SourcePath    string           `json:"source_path"`
	InputSHA256   string           `json:"input_sha256"`
	FileSize      int64            `json:"file_size"`
	Tracks        []TrackIndex     `json:"tracks"`
	Summaries     []TrackSummary   `json:"summaries"`
	MdatStart     int64            `json:"mdat_start"`
	MdatEnd       int64            `json:"mdat_end"`
	Errors        media.ErrorList  `json:"errors,omitempty"`
}

// IndexFileName is the content-addressed index name inside workDir/index.
func IndexFileName(inputSHA string) string { return inputSHA[:16] + ".index.json" }

// WriteIndex atomically writes the index under workDir/index.
func WriteIndex(workDir string, idx *FileIndex) (string, error) {
	dir, err := SecureDir(workDir, "index")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, IndexFileName(idx.InputSHA256))
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", media.NewError(media.KindIO, path, err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", media.NewError(media.KindIO, path, err.Error())
	}
	return path, nil
}

// ReadIndex loads an index file.
func ReadIndex(path string) (*FileIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewError(media.KindIO, path, err.Error())
	}
	var idx FileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("index %s: invalid JSON: %w", path, err)
	}
	return &idx, nil
}

// SecureDir creates and returns a sub-directory of workDir after verifying
// that it does not escape the work directory boundary.
func SecureDir(workDir, sub string) (string, error) {
	dir, err := SafeJoin(workDir, sub)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", media.NewError(media.KindIO, dir, err.Error())
	}
	return dir, nil
}

// HashFile streams SHA-256 over path without loading it into memory.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, media.NewError(media.KindIO, path, err.Error())
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyChunk(h, f)
	if err != nil {
		return "", 0, media.NewError(media.KindIO, path, err.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
