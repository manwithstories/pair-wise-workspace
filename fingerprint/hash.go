// Package fingerprint computes per-segment content hashes for duplicate
// detection and idempotent retry keys.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"bcast/media"
	"bcast/segments"
)

// ErrShortPayload reports that a sample's declared payload could not be read
// in full — typically a truncated file. The hash is not written for such a
// segment: a partial read must never masquerade as whole content.
var ErrShortPayload = errors.New("sample payload shorter than declared")

// Hash computes the content hash of one segment: SHA-256 over the sample
// payloads in track order (video, then audio), each framed with its size.
// Timestamps are excluded so duplicate content delivered at different
// positions hashes identically. A short read is an error, not a hash of
// whatever bytes happened to be present.
func Hash(src io.ReaderAt, seg segments.Segment) (string, error) {
	h := sha256.New()
	write := func(s media.Sample) error {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], s.Size)
		h.Write(lenBuf[:])
		n, err := io.CopyN(h, io.NewSectionReader(src, s.Offset, int64(s.Size)), int64(s.Size))
		if err != nil {
			return fmt.Errorf("%w: seg %d %s @%d: %v", ErrShortPayload,
				seg.Index, s.Kind, s.Offset, err)
		}
		if n != int64(s.Size) {
			return fmt.Errorf("%w: seg %d %s @%d: %d of %d bytes", ErrShortPayload,
				seg.Index, s.Kind, s.Offset, n, s.Size)
		}
		return nil
	}
	for _, s := range seg.Video {
		if err := write(s); err != nil {
			return "", err
		}
	}
	for _, s := range seg.Audio {
		if err := write(s); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Key builds the idempotency key for one segment task: input digest +
// parameter digest + segment ordinal. Re-executing the same task yields the
// same key, so results are written at most once.
func Key(inputDigest, params string, segIndex int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", inputDigest, params, segIndex)))
	return fmt.Sprintf("%x", sum[:16])
}

// InputDigest is a stable digest of input identity: callers pass the parts
// that define the bytes consumed (path, size, a content digest). It is not a
// digest of the whole file, so a 1 GiB delivery costs no extra I/O here.
func InputDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:16])
}

// Dedup groups segment ordinals by identical hash. Groups with more than one
// member are duplicates; the group lists indexes in ascending order because
// hashes are consumed in segment order.
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
