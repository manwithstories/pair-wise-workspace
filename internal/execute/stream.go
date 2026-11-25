// Package execute streams timestamp corrections into repaired fragments.
//
// Media payload is never transcoded or altered: only same-width tfdt
// value patches are written. Bulk I/O flows through one fixed window so
// memory stays bounded as fragments and indexes grow. Each chunk is
// published by temp-file + atomic rename, which makes resume safe: a
// final file is either absent or complete, and re-runs skip it instead
// of appending half-written data.
package execute

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archivectl/internal/index"
	"archivectl/internal/scan"
)

// WindowSize is the fixed streaming buffer size.
const WindowSize = 32 * 1024

// Patch is an in-place byte replacement at an absolute file offset.
type Patch struct {
	Offset int64
	Data   []byte
}

// TFDTYPatch builds the same-width binary patch that replaces a raw
// tfdt base time with the corrected native value. Width/version are
// preserved so box sizes and all payload offsets stay unchanged.
func TFDTYPatch(info scan.TFDTInfo, correctedNativeBase int64) (Patch, error) {
	p := Patch{Offset: info.ValueOffset}
	switch info.ValueWidth {
	case 8:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(correctedNativeBase))
		p.Data = b[:]
	case 4:
		if correctedNativeBase < 0 || correctedNativeBase > 0xFFFFFFFF {
			return Patch{}, fmt.Errorf("execute: corrected base %d does not fit tfdt v0", correctedNativeBase)
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(correctedNativeBase))
		p.Data = b[:]
	default:
		return Patch{}, fmt.Errorf("execute: unknown tfdt width %d", info.ValueWidth)
	}
	return p, nil
}

// RewriteFileAtomic streams srcPath into a same-directory temp file
// using one fixed buffer, applies in-place patches, fsyncs and atomically
// renames to dstPath. It returns the final size and SHA-256.
func RewriteFileAtomic(srcPath, dstPath string, patches []Patch) (int64, string, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, "", err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), filepath.Base(dstPath)+".*.tmp")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	fail := func(err error) (int64, string, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return 0, "", err
	}

	buf := make([]byte, WindowSize)
	if _, err := io.CopyBuffer(tmp, in, buf); err != nil {
		return fail(fmt.Errorf("execute: stream %s: %w", srcPath, err))
	}
	for _, p := range patches {
		if _, err := tmp.WriteAt(p.Data, p.Offset); err != nil {
			return fail(fmt.Errorf("execute: patch at %d: %w", p.Offset, err))
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return 0, "", err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		_ = os.Remove(tmpName)
		return 0, "", err
	}
	sha, err := index.SHA256File(dstPath)
	if err != nil {
		return st.Size(), "", err
	}
	return st.Size(), sha, nil
}

// ChunkComplete reports whether a final chunk from a previous run is
// present with the expected (same-length rewrite) size. Atomic rename
// makes a size-matching final file trustworthy to skip.
func ChunkComplete(dstPath string, expectedSize int64) bool {
	st, err := os.Stat(dstPath)
	return err == nil && !st.IsDir() && st.Size() == expectedSize
}

// CleanStaleTemp removes atomic-write leftovers ("*.tmp" or names
// containing ".tmp-") under dir before a resumed run.
func CleanStaleTemp(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".tmp") || strings.Contains(n, ".tmp-") {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
