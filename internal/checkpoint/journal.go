// Package checkpoint writes atomic recovery journals containing only
// finalized segments.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/timeline"
)

const Version = 1

// ErrCorruptCheckpoint marks an unreadable or semantically invalid journal.
var ErrCorruptCheckpoint = errors.New("corrupt checkpoint")

// SegmentRecord is one atomically committed, complete segment.
type SegmentRecord struct {
	ID         int    `json:"id"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	StartNS    int64  `json:"start_ns"`
	EndNS      int64  `json:"end_ns"`
	DurationNS int64  `json:"duration_ns"`
	AUIndices  []int  `json:"au_indices,omitempty"`
}

// Journal is the durable resume point.
type Journal struct {
	Version        int             `json:"version"`
	Codec          string          `json:"codec"`
	Seed           int64           `json:"seed"`
	TargetBPS      int             `json:"target_bps"`
	Segments       []SegmentRecord `json:"segments"`
	ManifestFile   string          `json:"manifest_file,omitempty"`
	ManifestSHA256 string          `json:"manifest_sha256,omitempty"`
	Completed      bool            `json:"completed"`
}

func NewJournal(codec string, seed int64, targetBPS int) *Journal {
	return &Journal{Version: Version, Codec: codec, Seed: seed, TargetBPS: targetBPS}
}

// Load reads a journal. A missing file yields an empty journal and exists=false.
// Invalid JSON or unsupported version is a classified storage error.
func Load(path string) (j *Journal, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, timeline.Wrap(timeline.KindStorage, "checkpoint.read", -1, -1, err)
	}
	var got Journal
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, true, &timeline.Error{
			Kind: timeline.KindStorage, Stage: "checkpoint.corrupt", Index: -1, Offset: -1,
			Err: fmt.Errorf("%w: %v", ErrCorruptCheckpoint, err),
		}
	}
	if got.Version != Version {
		return nil, true, &timeline.Error{
			Kind: timeline.KindStorage, Stage: "checkpoint.corrupt", Index: -1, Offset: -1,
			Err: fmt.Errorf("%w: unsupported version %d", ErrCorruptCheckpoint, got.Version),
		}
	}
	for i, rec := range got.Segments {
		if !rec.complete() {
			return nil, true, &timeline.Error{
				Kind: timeline.KindStorage, Stage: "checkpoint.corrupt", Index: i, Offset: -1,
				Err: fmt.Errorf("%w: incomplete segment record %+v", ErrCorruptCheckpoint, rec),
			}
		}
	}
	return &got, true, nil
}

func (r SegmentRecord) complete() bool {
	return r.File != "" && r.SHA256 != "" && r.Size > 0 && r.EndNS > r.StartNS && r.DurationNS > 0
}

// AddSegment appends only a complete segment record.
func (j *Journal) AddSegment(rec SegmentRecord) error {
	if !rec.complete() {
		return timeline.New(timeline.KindInternal, "checkpoint.add_segment", rec.ID, -1,
			"refusing to record incomplete segment: %+v", rec)
	}
	for _, old := range j.Segments {
		if old.ID == rec.ID {
			return timeline.New(timeline.KindInternal, "checkpoint.add_segment", rec.ID, -1,
				"duplicate segment id already journaled")
		}
	}
	j.Segments = append(j.Segments, rec)
	return nil
}

// SaveAtomic writes the journal to a temp file and renames it.
func (j *Journal) SaveAtomic(path string) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return timeline.Wrap(timeline.KindInternal, "checkpoint.marshal", -1, -1, err)
	}
	b = append(b, '\n')
	if err := AtomicWriteFile(path, b, 0o644); err != nil {
		return timeline.Wrap(timeline.KindStorage, "checkpoint.save", -1, -1, err)
	}
	return nil
}

// AtomicWriteFile writes data in the destination directory then atomically
// renames the temporary file over path.
func AtomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// SafePath confines name to root and rejects absolute paths or traversal.
func SafePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\x00") {
		return "", timeline.New(timeline.KindArgument, "checkpoint.safe_path", -1, -1,
			"unsafe file name %q", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", timeline.New(timeline.KindArgument, "checkpoint.safe_path", -1, -1,
			"path traversal in %q", name)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", timeline.Wrap(timeline.KindStorage, "checkpoint.abs_root", -1, -1, err)
	}
	full := filepath.Join(absRoot, clean)
	rel, err := filepath.Rel(absRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", timeline.New(timeline.KindArgument, "checkpoint.safe_path", -1, -1,
			"path %q escapes output root", name)
	}
	return full, nil
}

// HashFile computes SHA-256 and size for an existing file.
func HashFile(path string) (string, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), int64(len(b)), nil
}

// VerifyFiles checks every journaled segment against its recorded hash.
func (j *Journal) VerifyFiles(root string) error {
	for _, rec := range j.Segments {
		full, err := SafePath(root, rec.File)
		if err != nil {
			return err
		}
		sum, size, err := HashFile(full)
		if err != nil {
			return timeline.Wrap(timeline.KindStorage, "checkpoint.verify", rec.ID, -1, err)
		}
		if sum != rec.SHA256 || size != rec.Size {
			return &timeline.Error{
				Kind: timeline.KindStorage, Stage: "checkpoint.verify", Index: rec.ID, Offset: -1,
				Err: fmt.Errorf("%w: segment %d hash/size mismatch", ErrCorruptCheckpoint, rec.ID),
			}
		}
	}
	return nil
}
