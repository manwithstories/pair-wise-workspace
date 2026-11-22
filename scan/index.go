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
const ParserVersion = "mediaslice-parser/v1"

// SampleRecord is one indexed sample in unified 90 kHz ticks.
type SampleRecord struct {
	Index      int         `json:"i"`
	Offset     int64       `json:"off"`
	Size       int         `json:"size"`
	StartTick  media.Ticks `json:"start"`
	Duration   media.Ticks `json:"dur"`
	Keyframe   bool        `json:"key,omitempty"`
	// CorrectedTick is the drift-corrected RTP timeline position for tracks
	// that have an RTP sidecar.
	CorrectedTick media.Ticks `json:"corr,omitempty"`
}

// EditRecord is one retained edit-list entry in track timescale units.
type EditRecord struct {
	SegmentDuration int64 `json:"seg_dur"`
	MediaTime       int64 `json:"media_time"` // may be negative
	Rate            int32 `json:"rate"`
}

// RTPStats summarizes sidecar unwrap/drift processing.
type RTPStats struct {
	Packets          int             `json:"packets"`
	WrapCount        int             `json:"wraps"`
	TotalCorrection  media.Ticks     `json:"total_correction"`
	WrapEvents       []media.WrapEvent `json:"wraps_detail,omitempty"`
}

// TrackIndex is the per-track parse result.
type TrackIndex struct {
	TrackID   uint32          `json:"track_id"`
	Handler   string          `json:"handler"`
	Timescale uint32          `json:"timescale"`
	Edits     []EditRecord    `json:"edits,omitempty"`
	Samples   []SampleRecord  `json:"samples"`
	RTP       *RTPStats       `json:"rtp,omitempty"`
	// Stopped marks a track the scanner abandoned (e.g. unprovable wrap).
	Stopped bool           `json:"stopped,omitempty"`
	Errors  []*media.MediaError `json:"errors,omitempty"`
}

// TrackSummary is a compact digest embedded in the index header.
type TrackSummary struct {
	TrackID     uint32 `json:"track_id"`
	Handler     string `json:"handler"`
	SampleCount int    `json:"samples"`
	// SampleDigest covers offsets/sizes/timing of every sample.
	SampleDigest string `json:"digest"`
}

// FileIndex is the complete, verifiable scan result for one MP4 source.
type FileIndex struct {
	ParserVersion string         `json:"parser_version"`
	SourcePath    string         `json:"source_path"`
	InputSHA256   string         `json:"input_sha256"`
	FileSize      int64          `json:"file_size"`
	Tracks        []TrackIndex   `json:"tracks"`
	Summaries     []TrackSummary `json:"summaries"`
	// MdatStart/MdatEnd bound every validated sample byte range.
	MdatStart int64 `json:"mdat_start"`
	MdatEnd   int64 `json:"mdat_end"`
	Errors    media.ErrorList `json:"errors,omitempty"`
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
