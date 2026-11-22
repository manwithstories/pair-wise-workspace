// Package fault defines the unified damage/error model used by every
// subsystem. A Fault always records the source file, the raw byte offset
// inside that input (when known), the processing stage and whether the
// surrounding region can still be analyzed.
package fault

import (
	"errors"
	"fmt"
	"path/filepath"
)

// MaxInputSize is the supported upper bound for a single input: 2 GiB.
const MaxInputSize int64 = 2 << 30

// Stage identifies the pipeline stage that produced a Fault.
type Stage string

const (
	StageSubtitle   Stage = "subtitle"
	StageTimecode   Stage = "timecode"
	StageMP4        Stage = "mp4"
	StageRTP        Stage = "rtp"
	StagePipeline   Stage = "pipeline"
	StageCheckpoint Stage = "checkpoint"
)

// Fault is a single traced damage record. Offset is the raw byte offset in
// Source; -1 means the fault is not anchored to a byte position (for example
// a logical overlap between two subtitle records).
type Fault struct {
	Source      string `json:"source"`
	Offset      int64  `json:"offset"`
	Stage       Stage  `json:"stage"`
	Recoverable bool   `json:"recoverable"`
	Message     string `json:"message"`
}

// Error implements the error interface without losing structured fields.
func (f Fault) Error() string {
	if f.Offset < 0 {
		return fmt.Sprintf("%s: %s (recoverable=%v)", f.Stage, f.Message, f.Recoverable)
	}
	return fmt.Sprintf("%s: %s at %s:%d (recoverable=%v)",
		f.Stage, f.Message, filepath.Base(f.Source), f.Offset, f.Recoverable)
}

// New builds a Fault anchored at a raw byte offset.
func New(source string, offset int64, stage Stage, recoverable bool, format string, args ...any) Fault {
	return Fault{
		Source:      source,
		Offset:      offset,
		Stage:       stage,
		Recoverable: recoverable,
		Message:     fmt.Sprintf(format, args...),
	}
}

// CheckOffset rejects offsets that fall outside [0, size]. It exists so no
// subsystem can silently report a fabricated or out-of-range byte position.
func CheckOffset(source string, offset, size int64, stage Stage) (Fault, bool) {
	if offset < 0 || offset >= size {
		return New(source, offset, stage, false,
			"offset %d out of bounds for input of %d bytes", offset, size), false
	}
	return Fault{}, true
}

// CheckPath rejects absolute paths and any path escaping the allowed root
// via ".." segments. The cleaned path is returned on success.
func CheckPath(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute input path rejected: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || filepath.IsAbs(clean) ||
		len(clean) >= 3 && clean[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("path escapes input root: %s", name)
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("path escapes input root: %s", name)
	}
	return full, nil
}

// SafeAlloc allocates a byte buffer only when n is positive and within the
// 2 GiB input bound, preventing oversized allocations driven by input bytes.
func SafeAlloc(n int64) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("allocation size must be positive")
	}
	if n > MaxInputSize {
		return nil, fmt.Errorf("allocation of %d bytes exceeds 2 GiB limit", n)
	}
	return make([]byte, n), nil
}
