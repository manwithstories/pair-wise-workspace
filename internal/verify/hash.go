// Package verify provides deterministic hashing and atomic file publication.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/mediaerr"
)

// SHA256Bytes returns the lowercase hex SHA-256 of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SHA256File streams a file and returns its lowercase hex SHA-256.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyFile compares a file hash with wantHex.
func VerifyFile(path, wantHex string) error {
	got, err := SHA256File(path)
	if err != nil {
		return err
	}
	if got != wantHex {
		return mediaerr.New(mediaerr.KindOutputMismatch, -1, 0, false,
			"%s sha256=%s want %s", path, got, wantHex)
	}
	return nil
}

// AtomicWriteFile writes to a unique temporary file in the destination
// directory, fsyncs it, and renames it over the target. Readers therefore see
// either the old complete file or the new complete file.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	if err := tmp.Close(); err != nil {
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return mediaerr.Wrap(mediaerr.KindInvalidArgument, -1, 0, true, err)
	}
	cleanup = false
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

// IsNotExist reports whether err is an os.IsNotExist-compatible error.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
