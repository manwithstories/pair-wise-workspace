// Package framehash computes per-sample SHA-256 digests and an aggregate root
// hash over an interval, reading samples on demand without loading the file.
package framehash

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

// short16 returns the first 16 bytes (32 hex chars) of a digest.
func short16(sum [32]byte) string {
	return hex.EncodeToString(sum[:16])
}

// readSample reads exactly one sample's bytes through a bounded read.
func readSample(ra io.ReaderAt, off int64, size uint32) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := ra.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, report.NewError(report.ExitFileRead, "hash", err.Error())
	}
	return buf, nil
}

// FrameDigest is one sample's digest plus its location metadata.
type FrameDigest struct {
	Index int
	Sha   string
	Size  uint32
}

// HashRange hashes samples [start,end) of a track and returns per-frame
// digests, the aggregate root hash and the total sample bytes.
func HashRange(m *mp4atom.Movie, tr *mp4atom.Track, start, end int) (
	[]FrameDigest, string, int64, error) {
	n := len(tr.Samples)
	if start < 0 || end > n || start >= end {
		return nil, "", 0, report.NewError(report.ExitArgs, "hash",
			"range out of bounds")
	}
	ra := m.ReaderAt()
	root := sha256.New()
	out := make([]FrameDigest, 0, end-start)
	var total int64
	for i := start; i < end; i++ {
		s := tr.Samples[i]
		buf, err := readSample(ra, s.Offset, s.Size)
		if err != nil {
			return nil, "", 0, err
		}
		sum := sha256.Sum256(buf)
		root.Write(sum[:])
		out = append(out, FrameDigest{Index: i, Sha: short16(sum), Size: s.Size})
		total += int64(s.Size)
	}
	var rootSum [32]byte
	copy(rootSum[:], root.Sum(nil))
	return out, short16(rootSum), total, nil
}

// CompareRange aligns two equally sized track intervals sample-by-sample and
// counts matches and mismatches using raw sample content hashes.
func CompareRange(src, clip *mp4atom.Movie, srcTr, clipTr *mp4atom.Track,
	srcStart, clipStart, count int) (matched, mismatched int, err error) {
	if clipStart+count > len(clipTr.Samples) ||
		srcStart+count > len(srcTr.Samples) {
		return 0, 0, report.NewError(report.ExitArgs, "verify",
			"verify range exceeds available samples")
	}
	for k := 0; k < count; k++ {
		a := srcTr.Samples[srcStart+k]
		b := clipTr.Samples[clipStart+k]
		ab, err := readSample(src.ReaderAt(), a.Offset, a.Size)
		if err != nil {
			return 0, 0, err
		}
		bb, err := readSample(clip.ReaderAt(), b.Offset, b.Size)
		if err != nil {
			return 0, 0, err
		}
		if sha256.Sum256(ab) == sha256.Sum256(bb) {
			matched++
		} else {
			mismatched++
		}
	}
	return matched, mismatched, nil
}
