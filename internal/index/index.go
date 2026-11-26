// Package index builds, validates, and atomically persists archive indexes.
package index

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"archivectl/internal/clock"
	"archivectl/internal/core"
	"archivectl/internal/verify"
	"archivectl/internal/wavscan"
)

// Version is part of the index key. Bump it when normalized schema changes.
const Version = "archive-index/v1"

// Key is the invalidation envelope. It covers the format version plus digests
// of the WAV and clock inputs.
type Key struct {
	IndexVersion string `json:"index_version"`
	WAVSHA256    string `json:"wav_sha256"`
	ClockSHA256  string `json:"clock_sha256"`
	ChecksSHA256 string `json:"checks_sha256,omitempty"`
}

// Index is the normalized, serializable scan/repair plan.
type Index struct {
	Key `json:"key"`

	WAVPath    string `json:"wav_path"`
	ClockPath  string `json:"clock_path,omitempty"`
	ChecksPath string `json:"checks_path,omitempty"`

	SampleRate    int   `json:"sample_rate"`
	Channels      int   `json:"channels"`
	BitsPerSample int   `json:"bits_per_sample"`
	FrameBytes    int   `json:"frame_bytes"`
	DataOffset    int64 `json:"data_offset"`

	DeclaredFrames  int64 `json:"declared_frames"`
	AvailableFrames int64 `json:"available_frames"`
	ExpectedFrames  int64 `json:"expected_frames"`

	Cues    []wavscan.CuePoint       `json:"cues,omitempty"`
	Chunks  []wavscan.ChunkInfo      `json:"chunks,omitempty"`
	Anchors []clock.NormalizedAnchor `json:"anchors,omitempty"`
	Blocks  []clock.NormalizedBlock  `json:"blocks,omitempty"`

	BlockChecks []verify.BlockCheck `json:"block_checks,omitempty"`
	Damage      []core.Span         `json:"damage_spans,omitempty"`
}

// BuildInput contains all stage outputs that must survive into the index.
type BuildInput struct {
	WAVPath      string
	ClockPath    string
	ChecksPath   string
	Report       *wavscan.Report
	Anchors      []clock.Anchor
	BlockRecords []clock.BlockRecord
	BlockChecks  []verify.BlockCheck
	Damage       []core.Span
}

// Build hashes inputs, estimates clock drift, normalizes anchors/blocks, and
// merges structural and checksum damage into one canonical range list.
func Build(in BuildInput) (*Index, error) {
	const op = "index.Build"
	if in.Report == nil {
		return nil, core.New(core.KindInvalidArgument, op, "scan report is required")
	}
	wavDigest, err := verify.FileSHA256(in.WAVPath)
	if err != nil {
		return nil, err
	}
	clockDigest := ""
	if in.ClockPath != "" {
		clockDigest, err = verify.FileSHA256(in.ClockPath)
		if err != nil {
			return nil, err
		}
	}
	checksDigest := ""
	if in.ChecksPath != "" {
		checksDigest, err = verify.FileSHA256(in.ChecksPath)
		if err != nil {
			return nil, err
		}
	}

	var model clock.DriftModel
	var anchors []clock.NormalizedAnchor
	if len(in.Anchors) >= 2 {
		model, anchors, err = clock.EstimateDrift(in.Anchors)
		if err != nil {
			return nil, err
		}
	}
	blocks, err := clock.NormalizeBlockRecords(in.BlockRecords, model)
	if err != nil {
		return nil, err
	}

	idx := &Index{
		Key: Key{
			IndexVersion: Version,
			WAVSHA256:    wavDigest,
			ClockSHA256:  clockDigest,
			ChecksSHA256: checksDigest,
		},
		WAVPath:         in.WAVPath,
		ClockPath:       in.ClockPath,
		ChecksPath:      in.ChecksPath,
		SampleRate:      in.Report.Format.SampleRate,
		Channels:        in.Report.Format.Channels,
		BitsPerSample:   in.Report.Format.BitsPerSample,
		FrameBytes:      in.Report.Format.FrameBytes(),
		DataOffset:      in.Report.DataOffset,
		DeclaredFrames:  in.Report.DeclaredFrames,
		AvailableFrames: in.Report.AvailableFrames,
		ExpectedFrames:  in.Report.DeclaredFrames,
		Cues:            in.Report.Cues,
		Chunks:          in.Report.Chunks,
		Anchors:         anchors,
		Blocks:          blocks,
		BlockChecks:     in.BlockChecks,
	}
	if in.Report.AvailableFrames < in.Report.DeclaredFrames {
		idx.Damage = append(idx.Damage, core.Span{
			Kind:        "truncation",
			StartFrame:  in.Report.AvailableFrames,
			EndFrame:    in.Report.DeclaredFrames,
			Recoverable: in.Report.DeclaredFrames-in.Report.AvailableFrames <= int64(core.MaxCachedFrames),
			Source:      "riff-data-length",
			Detail:      "PCM data extends beyond readable file",
		})
	}
	idx.Damage = core.MergeSpans(append(idx.Damage, in.Damage...))
	return idx, nil
}

// SaveAtomic writes indented JSON to a temp file in path's directory and
// renames it over path. Readers never observe a partially written index.
func (idx *Index) SaveAtomic(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".index-*.tmp")
	if err != nil {
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(idx); err != nil {
		_ = tmp.Close()
		cleanup()
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return core.Wrap(core.KindIO, "index.SaveAtomic", path, err)
	}
	return nil
}

// Load reads a persisted index.
func Load(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Wrap(core.KindIO, "index.Load", path, err)
	}
	defer f.Close()
	var idx Index
	if err := json.NewDecoder(f).Decode(&idx); err != nil {
		return nil, core.Wrap(core.KindIndexCorrupt, "index.Load", path, err)
	}
	if idx.IndexVersion != Version {
		return nil, core.Newf(core.KindIndexStale, "index.Load",
			"version %q != %q", idx.IndexVersion, Version)
	}
	return &idx, nil
}

// ValidateInputs recomputes input digests. Any WAV or clock change makes the
// index stale and unsafe to execute.
func (idx *Index) ValidateInputs(wavPath, clockPath string) error {
	const op = "index.ValidateInputs"
	if idx.IndexVersion != Version {
		return core.Newf(core.KindIndexStale, op, "version changed to %q", Version)
	}
	wavDigest, err := verify.FileSHA256(wavPath)
	if err != nil {
		return err
	}
	if wavDigest != idx.WAVSHA256 {
		return core.New(core.KindIndexStale, op, "WAV digest differs from index key")
	}
	if clockPath != "" {
		d, err := verify.FileSHA256(clockPath)
		if err != nil {
			return err
		}
		want := idx.ClockSHA256
		if d != want {
			return core.New(core.KindIndexStale, op, "clock record digest differs from index key")
		}
	}
	return nil
}

// WriteAtomic copies r into path via a same-directory temp file and rename.
// Repair uses this so a racing input never causes a half-committed output.
func WriteAtomic(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".output-*.tmp")
	if err != nil {
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		cleanup()
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return core.Wrap(core.KindIO, "index.WriteAtomic", path, err)
	}
	return nil
}
