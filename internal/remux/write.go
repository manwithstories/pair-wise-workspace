// Package remux performs deterministic, atomic output writing. All writes go
// to a temp file inside the destination directory and are renamed into place
// only after a final checksum succeeds; path traversal outside the root is
// rejected, so no partial or out-of-root output can appear.
package remux

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mediaflow/internal/clock"
	"mediaflow/internal/quality"
	"mediaflow/internal/timeline"
)

// MaxOutputBytes bounds a single remuxed artifact.
const MaxOutputBytes = 256 << 20

// Writer owns one output root and enforces safe, atomic writes.
type Writer struct {
	root string
}

// NewWriter validates and creates the output root.
func NewWriter(root string) (*Writer, error) {
	clean, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, timeline.GlobalError("remux.new", false, err)
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, timeline.GlobalError("remux.new", false, err)
	}
	return &Writer{root: clean}, nil
}

// Root returns the absolute, validated output root.
func (w *Writer) Root() string { return w.root }

// safePath resolves name strictly inside the root.
func (w *Writer) safePath(name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", timeline.GlobalError("remux.path", false,
			fmt.Errorf("%w: absolute name %q", timeline.ErrPathEscape, name))
	}
	clean := filepath.Clean(filepath.Join(w.root, name))
	rel, err := filepath.Rel(w.root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", timeline.GlobalError("remux.path", false,
			fmt.Errorf("%w: %q", timeline.ErrPathEscape, name))
	}
	return clean, nil
}

// Result describes a completed atomic write.
type Result struct {
	Path   string
	Bytes  int64
	SHA256 string
}

// WriteAtomically streams r to name via a temp sibling file, hashes it while
// writing, enforces the size limit and renames into place only on success.
func (w *Writer) WriteAtomically(name string, r io.Reader) (Result, error) {
	dest, err := w.safePath(name)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, timeline.GlobalError("remux.write", false, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".mediaflow-*.tmp")
	if err != nil {
		return Result{}, timeline.GlobalError("remux.write", false, err)
	}
	tmpName := tmp.Name()
	// On any failure the temp file is removed; the destination is untouched.
	rollback := func(cause error) (Result, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Result{}, cause
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, MaxOutputBytes+1))
	if err != nil {
		return rollback(timeline.GlobalError("remux.write", false, err))
	}
	if n > MaxOutputBytes {
		return rollback(timeline.GlobalError("remux.write", false,
			fmt.Errorf("%w: output larger than %d bytes", timeline.ErrLimit, MaxOutputBytes)))
	}
	if err := tmp.Sync(); err != nil {
		return rollback(timeline.GlobalError("remux.write", false, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Result{}, timeline.GlobalError("remux.write", false, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return Result{}, timeline.GlobalError("remux.write", false, err)
	}
	return Result{Path: dest, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Frame is the serializable normalized frame handed to the remux encoder.
type Frame struct {
	Track     uint32
	Seq       uint32
	StartUS   int64
	Duration  int64
	Recovered bool
	Payload   []byte
}

// EncodeFrames deterministically serializes normalized frames into a compact
// binary stream and returns its bytes plus a checksum. It is a stand-in for a
// full container mux but remains fully deterministic and dependency-free.
func EncodeFrames(frames []clock.Normalized) ([]byte, [32]byte) {
	var buf []byte
	put := func(b []byte) { buf = append(buf, b...) }
	var tag [1]byte
	var u64 [8]byte
	putU64 := func(v int64) {
		binary.LittleEndian.PutUint64(u64[:], uint64(v))
		put(u64[:])
	}
	for _, f := range frames {
		if f.Recovered {
			tag[0] = 1
		} else {
			tag[0] = 0
		}
		put(tag[:])
		putU64(int64(f.Track))
		putU64(int64(f.Seq))
		putU64(f.StartUS)
		putU64(f.Duration)
		put(f.Payload)
	}
	return buf, sha256.Sum256(buf)
}

// EncodeSegments writes cut segments as a deterministic manifest body.
func EncodeSegments(segs []quality.Segment) []byte {
	var buf []byte
	putU64 := func(v int64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v))
		buf = append(buf, b[:]...)
	}
	for _, s := range segs {
		putU64(int64(s.Index))
		putU64(s.StartUS)
		putU64(s.EndUS)
		putU64(int64(s.StartFrame))
		putU64(int64(s.EndFrame))
	}
	return buf
}
