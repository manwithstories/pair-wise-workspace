// Package verify hashes files and frame-aligned PCM segments and compares
// repaired output against generator-produced baselines.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"archivectl/internal/core"
)

const SegmentFrames = 4096

// SegmentHash identifies one fixed-frame shard.
type SegmentHash struct {
	Index      int    `json:"index"`
	StartFrame int64  `json:"start_frame"`
	FrameCount int    `json:"frame_count"`
	SHA256     string `json:"sha256"`
}

// FileSHA256 returns the hex SHA-256 of a complete local file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", core.Wrap(core.KindIO, "verify.FileSHA256", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", core.Wrap(core.KindIO, "verify.FileSHA256", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BytesSHA256 returns the hex digest of b.
func BytesSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PCMSegmentHashes hashes frame-aligned regions from a ReaderAt. Each shard
// contains at most SegmentFrames frames; the final shard may be shorter.
func PCMSegmentHashes(r io.ReaderAt, dataOffset int64, frameBytes int, totalFrames int64) ([]SegmentHash, error) {
	const op = "verify.PCMSegmentHashes"
	if frameBytes <= 0 {
		return nil, core.Newf(core.KindInvalidArgument, op, "frame bytes %d invalid", frameBytes)
	}
	if totalFrames < 0 {
		return nil, core.Newf(core.KindInvalidArgument, op, "total frames %d invalid", totalFrames)
	}
	var out []SegmentHash
	buf := make([]byte, SegmentFrames*frameBytes)
	for start := int64(0); start < totalFrames; start += SegmentFrames {
		nFrames := int64(SegmentFrames)
		if start+nFrames > totalFrames {
			nFrames = totalFrames - start
		}
		nBytes := nFrames * int64(frameBytes)
		if err := core.CheckAllocation(op, nBytes); err != nil {
			return nil, err
		}
		region := buf[:nBytes]
		off := dataOffset + start*int64(frameBytes)
		n, err := r.ReadAt(region, off)
		if err != nil && err != io.EOF {
			return nil, core.Wrap(core.KindIO, op, "", err)
		}
		if int64(n) != nBytes {
			return nil, core.Newf(core.KindVerification, op,
				"segment at frame %d short read: %d/%d bytes", start, n, nBytes)
		}
		out = append(out, SegmentHash{
			Index:      len(out),
			StartFrame: start,
			FrameCount: int(nFrames),
			SHA256:     BytesSHA256(region),
		})
	}
	return out, nil
}

// HashWAVPath opens path and hashes its PCM data using caller-supplied scan
// metadata. Keeping metadata explicit avoids an import cycle with wavscan.
func HashWAVPath(path string, dataOffset int64, frameBytes int, totalFrames int64) ([]SegmentHash, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Wrap(core.KindIO, "verify.HashWAVPath", path, err)
	}
	defer f.Close()
	return PCMSegmentHashes(f, dataOffset, frameBytes, totalFrames)
}

// Comparison is the per-shard verification result.
type Comparison struct {
	Total   int             `json:"total"`
	Matched int             `json:"matched"`
	Results []SegmentResult `json:"results"`
}

type SegmentResult struct {
	Index    int    `json:"index"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Match    bool   `json:"match"`
}

// CompareSegments compares shards by index and reports exact per-shard status.
func CompareSegments(actual, expected []SegmentHash) Comparison {
	n := len(expected)
	if len(actual) > n {
		n = len(actual)
	}
	c := Comparison{Total: n, Results: make([]SegmentResult, 0, n)}
	for i := 0; i < n; i++ {
		var a, e string
		if i < len(actual) {
			a = actual[i].SHA256
		}
		if i < len(expected) {
			e = expected[i].SHA256
		}
		match := a != "" && a == e
		if match {
			c.Matched++
		}
		c.Results = append(c.Results, SegmentResult{Index: i, Expected: e, Actual: a, Match: match})
	}
	return c
}

// AllMatch returns nil when every compared shard matched.
func (c Comparison) AllMatch() error {
	if c.Matched == c.Total && c.Total > 0 {
		return nil
	}
	return core.Newf(core.KindVerification, "verify.Comparison",
		"segment hash mismatch: %d/%d matched", c.Matched, c.Total)
}
