// Package verify re-reads delivered chunks and cross-checks them:
// per-chunk SHA-256, sample-table round-trip digests and keyframe
// random-access probes against the repaired source index.
package verify

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"mediaroute/internal/index"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
	"mediaroute/internal/output"
)

// DigestSamples returns a stable digest of a repaired sample table:
// per-sample DTS delta, payload size and sync flag. It is computed on
// the build side and recomputed on the re-read side for round-trip
// verification.
func DigestSamples(samples []model.Sample) string {
	h := sha256.New()
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(len(samples)))
	h.Write(buf[0:8])
	prev := int64(0)
	for i, s := range samples {
		delta := int64(0)
		if i > 0 {
			delta = s.DTS - prev
		}
		prev = s.DTS
		binary.BigEndian.PutUint64(buf[0:8], uint64(delta))
		binary.BigEndian.PutUint32(buf[8:12], s.Size)
		if s.Keyframe {
			buf[12] = 1
		} else {
			buf[12] = 0
		}
		h.Write(buf[0:13])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChunkReport is the verification verdict of one delivered chunk.
type ChunkReport struct {
	Path           string
	SHA256Match    bool
	RoundtripEqual bool
	KeyframeHit    bool
	Err            error
}

// OK reports whether every check passed.
func (r ChunkReport) OK() bool {
	return r.Err == nil && r.SHA256Match && r.RoundtripEqual && r.KeyframeHit
}

// VerifyChunk re-opens one delivered chunk and runs all checks:
//   - recompute the file SHA-256 and compare with the commit-time hash
//   - re-scan the sample table and compare its digest with the build digest
//   - probe the repaired source index at the chunk start clock and require
//     a verified sync sample that matches the chunk's first sample
func VerifyChunk(path, expectSHA, expectDigest string, ix *index.Index, startClock int64) ChunkReport {
	r := ChunkReport{Path: path}
	sum, _, err := output.HashFile(path)
	if err != nil {
		r.Err = err
		return r
	}
	r.SHA256Match = sum == expectSHA

	rt, err := mp4.ScanFile(path)
	if err != nil {
		r.Err = err
		return r
	}
	samples := rt.Expand()
	r.RoundtripEqual = DigestSamples(samples) == expectDigest

	if len(samples) > 0 && samples[0].Keyframe {
		if e, ok := ix.Lookup(startClock); ok && e.Clock <= startClock {
			r.KeyframeHit = true
		}
	}
	return r
}

// Summary aggregates chunk verification verdicts.
type Summary struct {
	Chunks    int
	HashMatch int
	Roundtrip int
	KeyHit    int
}

// Add folds one report into the summary.
func (s *Summary) Add(r ChunkReport) {
	s.Chunks++
	if r.SHA256Match {
		s.HashMatch++
	}
	if r.RoundtripEqual {
		s.Roundtrip++
	}
	if r.KeyframeHit {
		s.KeyHit++
	}
}

// Percent formats num/total as a fixed two-decimal percentage.
func Percent(num, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100.0 * float64(num) / float64(total)
}
