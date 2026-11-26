// Package report computes deterministic hashes and acceptance metrics.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"medialab/internal/index"
)

// SegmentResult is one per-segment hash and equality verdict.
type SegmentResult struct {
	Index int
	Hash  string
	Match bool
	State string
}

// Metrics contains all terminal acceptance counters.
type Metrics struct {
	RoundtripPassed   int
	RoundtripTotal    int
	KeyframeHits      int
	KeyframeTotal     int
	SegmentMatched    int
	SegmentTotal      int
	DuplicateDropped  int
	ConflictsIsolated int
	ReorderedPackets  int
	BurstRecovered    int
	BurstLost         int
	UnrecoverableLoss int
	AVSkewMS          int64
	FirstErrorOffset  int64
	BandwidthTargets  []int
	IndexSHA256       string
	Notes             []string
}

// Percent returns passed/total*100 with exact two-decimal formatting.
func Percent(passed, total int) string {
	if total == 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", float64(passed)*100/float64(total))
}

// RoundtripConsistency formats the roundtrip metric.
func (m Metrics) RoundtripConsistency() string {
	return Percent(m.RoundtripPassed, m.RoundtripTotal)
}

// KeyframeIndexHit formats the keyframe lookup metric.
func (m Metrics) KeyframeIndexHit() string {
	return Percent(m.KeyframeHits, m.KeyframeTotal)
}

// BurstRecovery formats recovered/lost.
func (m Metrics) BurstRecovery() string {
	return fmt.Sprintf("%d/%d", m.BurstRecovered, m.BurstLost)
}

// HashBytes returns the hex SHA-256 of payload.
func HashBytes(p []byte) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

// VerifySegments compares expected source bytes with recovered bytes and hashes
// every recovered fragment independently.
func VerifySegments(expected, recovered [][]byte, states []string) []SegmentResult {
	n := len(expected)
	if len(recovered) > n {
		n = len(recovered)
	}
	results := make([]SegmentResult, 0, n)
	for i := 0; i < n; i++ {
		var exp, got []byte
		if i < len(expected) {
			exp = expected[i]
		}
		if i < len(recovered) {
			got = recovered[i]
		}
		state := ""
		if i < len(states) {
			state = states[i]
		}
		results = append(results, SegmentResult{
			Index: i,
			Hash:  HashBytes(got),
			Match: string(exp) == string(got) && len(exp) > 0,
			State: state,
		})
	}
	return results
}

// MatchedCount counts matching segment hashes.
func MatchedCount(rs []SegmentResult) int {
	n := 0
	for _, r := range rs {
		if r.Match {
			n++
		}
	}
	return n
}

// HashKeyframeIndex hashes the canonical sorted sample list. Equal seeds and
// generated inputs therefore produce equal index hashes.
func HashKeyframeIndex(idx *index.Index) string {
	h := sha256.New()
	for _, e := range idx.Entries {
		fmt.Fprintf(h, "%d:%d:%d\n", e.Sample, e.Order, e.PTS.Milliseconds())
	}
	return hex.EncodeToString(h.Sum(nil))
}
