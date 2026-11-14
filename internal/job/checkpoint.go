package job

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SegmentCheckpoint records one finished segment on disk.
type SegmentCheckpoint struct {
	Segment int     `json:"segment"` // 1-based
	Frames  int     `json:"frames"`
	QP      int     `json:"qp"`
	Bits    int64   `json:"bits"`
	PSNR    float64 `json:"psnr_db"`
	Hash    string  `json:"hash"` // sha256 of the segment's source frames
}

// Meta is the job-level manifest persisted in the job directory.
type Meta struct {
	Input         string `json:"input"`
	InputSHA256   string `json:"input_sha256"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	TargetBps     int64  `json:"target_bps"`
	SegmentFrames int    `json:"segment_frames"`
	TotalFrames   int    `json:"total_frames"`
	Segments      int    `json:"segments"`
	State         string `json:"state"`
	Poisoned      []int  `json:"poisoned,omitempty"`
}

const (
	metaFile    = "job.json"
	segmentsDir = "segments"
)

// writeAtomic writes data to dir/name via a temp file and rename.
func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// WriteCheckpoint atomically persists a segment checkpoint.
func WriteCheckpoint(jobDir string, ck *SegmentCheckpoint) error {
	data, err := json.MarshalIndent(ck, "", "  ")
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%03d.chk", ck.Segment)
	return writeAtomic(filepath.Join(jobDir, segmentsDir), name, data)
}

// ReadCheckpoint loads a segment checkpoint; error if missing or corrupt.
func ReadCheckpoint(jobDir string, segment int) (*SegmentCheckpoint, error) {
	path := filepath.Join(jobDir, segmentsDir, fmt.Sprintf("%03d.chk", segment))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ck SegmentCheckpoint
	if err := json.Unmarshal(data, &ck); err != nil {
		return nil, fmt.Errorf("corrupt checkpoint %s: %w", path, err)
	}
	if ck.Segment != segment {
		return nil, fmt.Errorf("checkpoint segment mismatch: want %d got %d", segment, ck.Segment)
	}
	return &ck, nil
}

// WriteMeta atomically persists the job manifest.
func WriteMeta(jobDir string, m *Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(jobDir, metaFile, data)
}

// ReadMeta loads the job manifest.
func ReadMeta(jobDir string) (*Meta, error) {
	data, err := os.ReadFile(filepath.Join(jobDir, metaFile))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("corrupt job manifest: %w", err)
	}
	return &m, nil
}

// HashFrames hashes concatenated frame buffers.
func HashFrames(frames ...[]byte) string {
	h := sha256.New()
	for _, f := range frames {
		h.Write(f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashSegmentSource hashes frames [start,end) read from r.
func HashSegmentSource(r interface {
	ReadFrame(int, []byte) error
}, start, end, frameSize int) (string, error) {
	h := sha256.New()
	buf := make([]byte, frameSize)
	for i := start; i < end; i++ {
		if err := r.ReadFrame(i, buf); err != nil {
			return "", err
		}
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
