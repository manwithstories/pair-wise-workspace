// Package store persists the resumable state of a normalisation run.
//
// A checkpoint binds the input fingerprint, the configuration digest, the
// adapter versions and the number of completed output blocks. It is written
// with a temporary file plus atomic rename, so a crash leaves either the old
// checkpoint or the new one, never a torn file. Output blocks use the same
// discipline.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"mediacore/internal/media"
)

// CheckpointVersion is the on-disk schema revision.
const CheckpointVersion = 1

// BlockRecord describes one completed output block.
type BlockRecord struct {
	Index        int     `json:"index"`
	StartFrame   int64   `json:"start_frame"`
	Frames       int     `json:"frames"`
	Bytes        int64   `json:"bytes"`
	SHA256       string  `json:"sha256"`
	RTPTimestamp uint32  `json:"rtp_timestamp"`
	RTPSequence  uint16  `json:"rtp_sequence"`
	PeakDBFS     float64 `json:"peak_dbfs"`
}

// Checkpoint is the serialised resumable state.
type Checkpoint struct {
	Version          int     `json:"version"`
	InputPath        string  `json:"input_path"`
	InputSHA256      string  `json:"input_sha256"`
	InputBytes       int64   `json:"input_bytes"`
	ConfigDigest     string  `json:"config_digest"`
	ContainerVersion string  `json:"container_version"`
	CodecVersion     string  `json:"codec_version"`
	OutputRate       int     `json:"output_rate"`
	OutputChannels   int     `json:"output_channels"`
	LoudnessDBFS     float64 `json:"loudness_dbfs"`
	BlockFrames      int     `json:"block_frames"`

	CompletedBlocks int           `json:"completed_blocks"`
	TotalBlocks     int           `json:"total_blocks"`
	SamplesIn       int64         `json:"samples_in"`
	FramesOut       int64         `json:"frames_out"`
	PeakDBFS        float64       `json:"peak_dbfs"`
	Blocks          []BlockRecord `json:"blocks"`

	// Cursor is the number of *input* frames fully absorbed into the
	// completed blocks. Resume restarts the resampler at exactly this
	// input frame, so the deterministic kernel reproduces the same tail
	// state it had when the checkpoint was taken.
	Cursor int64 `json:"cursor"`
	// CanonicalSHA256 is the digest of the expected complete output,
	// computed on the first run from the sample table alone.
	CanonicalSHA256 string `json:"canonical_sha256"`
	// Stats count blocks rejected by the loudness stage.
	SilentBlocks  int `json:"silent_blocks"`
	ClippedBlocks int `json:"clipped_blocks"`
	LimitedBlocks int `json:"limited_blocks"`

	PartialSHA256 string `json:"partial_sha256"`
	Complete      bool   `json:"complete"`
	UpdatedAt     string `json:"updated_at"`
}

// Path returns the checkpoint file path for a work directory.
func Path(dir string) string { return filepath.Join(dir, "checkpoint.json") }

// BlocksDir returns the output-block directory for a work directory.
func BlocksDir(dir string) string { return filepath.Join(dir, "blocks") }

// OutputPath returns the assembled output path.
func OutputPath(dir string) string { return filepath.Join(dir, "out.pcm") }

// BlockPath returns the path of block index.
func BlockPath(dir string, index int) string {
	return filepath.Join(BlocksDir(dir), fmt.Sprintf("block-%05d.pcm", index))
}

// Load reads and validates the checkpoint schema. A missing file is not an
// error: it returns (nil, nil) so the caller can start fresh.
func Load(dir string) (*Checkpoint, error) {
	data, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, media.NewError(media.CodeCheckpointCorrupt, -1, 0, "read checkpoint: %v", err).Wrap(err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, media.NewError(media.CodeCheckpointCorrupt, -1, 0, "decode checkpoint: %v", err).Wrap(err)
	}
	if cp.Version != CheckpointVersion {
		return nil, media.NewError(media.CodeCheckpointCorrupt, -1, 0,
			"checkpoint version %d, expected %d", cp.Version, CheckpointVersion)
	}
	if cp.CompletedBlocks < 0 || cp.CompletedBlocks != len(cp.Blocks) {
		return nil, media.NewError(media.CodeCheckpointCorrupt, -1, 0,
			"checkpoint block count %d disagrees with %d records", cp.CompletedBlocks, len(cp.Blocks))
	}
	return &cp, nil
}

// Save writes cp atomically: temp file in the same directory, fsync, rename,
// then fsync the directory so the rename itself is durable.
func Save(dir string, cp *Checkpoint) error {
	cp.Version = CheckpointVersion
	cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "encode checkpoint: %v", err).Wrap(err)
	}
	data = append(data, '\n')
	if err := WriteFileAtomic(Path(dir), data, 0o644); err != nil {
		return err
	}
	return nil
}

// WriteBlockAtomically writes block bytes and records them in a temporary
// file renamed into place. The returned record carries the content digest.
func WriteBlockAtomically(dir string, index int, data []byte) (BlockRecord, error) {
	rec := BlockRecord{Index: index, Bytes: int64(len(data)), SHA256: Digest(data)}
	if err := WriteFileAtomic(BlockPath(dir, index), data, 0o644); err != nil {
		return rec, err
	}
	return rec, nil
}

// ReadBlock returns a completed block's bytes.
func ReadBlock(dir string, index int) ([]byte, error) {
	data, err := os.ReadFile(BlockPath(dir, index))
	if err != nil {
		return nil, media.NewError(media.CodeCheckpointCorrupt, -1, 0,
			"read block %d: %v", index, err).Wrap(err)
	}
	return data, nil
}

// VerifyBlocks re-reads every recorded block, checks its digest and returns
// the running digest of their concatenation. This is what makes resume trust
// only blocks that are actually intact on disk.
func VerifyBlocks(dir string, recs []BlockRecord) (string, error) {
	h := sha256.New()
	for _, rec := range recs {
		data, err := ReadBlock(dir, rec.Index)
		if err != nil {
			return "", err
		}
		if got := Digest(data); got != rec.SHA256 {
			return "", media.NewError(media.CodeCheckpointCorrupt, -1, 0,
				"block %d digest %s, checkpoint records %s", rec.Index, got, rec.SHA256)
		}
		if int64(len(data)) != rec.Bytes {
			return "", media.NewError(media.CodeCheckpointCorrupt, -1, 0,
				"block %d is %d bytes, checkpoint records %d", rec.Index, len(data), rec.Bytes)
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Assemble concatenates all blocks into out.pcm atomically and verifies that
// the resulting digest matches the expected running digest.
func Assemble(dir string, recs []BlockRecord, expected string) (string, error) {
	out := OutputPath(dir)
	tmp, err := os.CreateTemp(dir, ".out-*.part")
	if err != nil {
		return "", media.NewError(media.CodeAtomicWrite, -1, 0, "create output temp: %v", err).Wrap(err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	for _, rec := range recs {
		data, err := ReadBlock(dir, rec.Index)
		if err != nil {
			return "", err
		}
		if _, err := w.Write(data); err != nil {
			return "", media.NewError(media.CodeAtomicWrite, -1, 0, "write output: %v", err).Wrap(err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return "", media.NewError(media.CodeAtomicWrite, -1, 0, "sync output: %v", err).Wrap(err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expected != "" && got != expected {
		return "", media.NewError(media.CodeOutputVerification, -1, 0,
			"assembled digest %s, expected %s", got, expected)
	}
	if err := tmp.Close(); err != nil {
		return "", media.NewError(media.CodeAtomicWrite, -1, 0, "close output: %v", err).Wrap(err)
	}
	if err := os.Rename(tmpName, out); err != nil {
		return "", media.NewError(media.CodeAtomicWrite, -1, 0, "rename output: %v", err).Wrap(err)
	}
	ok = true
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return got, nil
}

// WriteFileAtomic writes data to path via a sibling temp file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.part")
	if err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "create temp for %s: %v", path, err).Wrap(err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "write temp: %v", err).Wrap(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "chmod temp: %v", err).Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "sync temp: %v", err).Wrap(err)
	}
	if err := tmp.Close(); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "close temp: %v", err).Wrap(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "rename %s: %v", path, err).Wrap(err)
	}
	ok = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "open dir %s: %v", dir, err).Wrap(err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "sync dir %s: %v", dir, err).Wrap(err)
	}
	return nil
}

// Digest returns the hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Binding is everything a resume must match exactly.
type Binding struct {
	InputSHA256      string
	ConfigDigest     string
	ContainerVersion string
	CodecVersion     string
	OutputRate       int
	OutputChannels   int
	LoudnessDBFS     float64
	BlockFrames      int
}

// Conflict explains why a checkpoint cannot be resumed.
type Conflict struct {
	Field   string
	Stored  string
	Current string
}

// Check reports whether cp matches b. A nil checkpoint is always compatible.
// The returned error is a *media.Error with CodeCheckpointConflict and a
// detail listing every differing field, so an operator sees all causes at
// once rather than fixing them one run at a time.
func Check(cp *Checkpoint, b Binding) error {
	if cp == nil {
		return nil
	}
	var diffs []Conflict
	add := func(field, stored, current string) {
		if stored != current {
			diffs = append(diffs, Conflict{Field: field, Stored: stored, Current: current})
		}
	}
	add("input_sha256", cp.InputSHA256, b.InputSHA256)
	add("config_digest", cp.ConfigDigest, b.ConfigDigest)
	add("container_version", cp.ContainerVersion, b.ContainerVersion)
	add("codec_version", cp.CodecVersion, b.CodecVersion)
	add("output_rate", itoa(cp.OutputRate), itoa(b.OutputRate))
	add("output_channels", itoa(cp.OutputChannels), itoa(b.OutputChannels))
	add("loudness_dbfs", ftoa(cp.LoudnessDBFS), ftoa(b.LoudnessDBFS))
	add("block_frames", itoa(cp.BlockFrames), itoa(b.BlockFrames))
	if len(diffs) == 0 {
		return nil
	}
	msg := ""
	for i, d := range diffs {
		if i > 0 {
			msg += "; "
		}
		msg += d.Field + " stored=" + d.Stored + " current=" + d.Current
	}
	return media.NewError(media.CodeCheckpointConflict, -1, 0, "%s", msg)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}

func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', 3, 64)
}
