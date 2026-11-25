// Package index computes whole-input SHA-256 digests and stores the
// deterministic chunk/sample boundary index used for random access.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// streamBufferSize bounds memory used while hashing or copying: all
// large I/O moves through one fixed-size window.
const streamBufferSize = 64 * 1024

// SHA256File streams a file through a fixed buffer and returns its hex
// digest without loading it wholly into memory.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, streamBufferSize)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256Bytes returns the hex digest of b.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DigestTree hashes every regular file under root in a deterministic
// order. Relative path, size and content are all fed to the hasher, so
// adding, deleting, renaming or modifying any input invalidates it.
func DigestTree(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Atomic-write leftovers and prior outputs never count as inputs.
		if isTempOrOutput(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	buf := make([]byte, streamBufferSize)
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(full)
		if err != nil {
			return "", err
		}
		// Path and length header delimit each record.
		io.WriteString(h, "file:")
		io.WriteString(h, rel)
		io.WriteString(h, ":")
		io.WriteString(h, itoa(st.Size()))
		io.WriteString(h, "\n")
		f, err := os.Open(full)
		if err != nil {
			return "", err
		}
		_, copyErr := io.CopyBuffer(h, f, buf)
		f.Close()
		if copyErr != nil {
			return "", copyErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isTempOrOutput(name string) bool {
	if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".tmp-") {
		return true
	}
	return false
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
