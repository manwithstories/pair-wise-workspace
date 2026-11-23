// Package fingerprint computes per-segment content hashes for duplicate
// detection and idempotent retry keys.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"bcast/segments"
)

// Hash computes the content hash of one segment: SHA-256 over the sample
// payloads in track order (video, then audio), each framed with its size.
// Timestamps are excluded so duplicate content delivered at different
// positions hashes identically.
func Hash(src io.ReaderAt, seg segments.Segment) (string, error) {
	h := sha256.New()
	write := func(off int64, size uint32) error {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], size)
		h.Write(lenBuf[:])
		_, err := io.CopyN(h, io.NewSectionReader(src, off, int64(size)), int64(size))
		return err
	}
	for _, s := range seg.Video {
		if err := write(s.Offset, s.Size); err != nil {
			return "", fmt.Errorf("fingerprint seg %d video @%d: %w", seg.Index, s.Offset, err)
		}
	}
	for _, s := range seg.Audio {
		if err := write(s.Offset, s.Size); err != nil {
			return "", fmt.Errorf("fingerprint seg %d audio @%d: %w", seg.Index, s.Offset, err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Key builds the idempotency key for one segment task:
// input digest + parameter digest + segment ordinal. Re-executing the same
// task yields the same key, so results are written at most once.
func Key(inputDigest, params string, segIndex int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", inputDigest, params, segIndex)))
	return fmt.Sprintf("%x", sum[:16])
}

// InputDigest hashes whole-input identity (path + size + content prefix is
// avoided; callers pass a stable digest of the input bytes).
func InputDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:16])
}

// Dedup groups segment ordinals by identical hash. Groups with more than
// one member are duplicates.
func Dedup(hashes []string) map[string][]int {
	groups := map[string][]int{}
	for i, h := range hashes {
		groups[h] = append(groups[h], i)
	}
	for h, idxs := range groups {
		if len(idxs) < 2 {
			delete(groups, h)
		}
	}
	return groups
}
