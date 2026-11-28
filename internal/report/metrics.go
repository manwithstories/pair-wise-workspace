// Package report aggregates run metrics and builds the frame manifest.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// FrameRecord is the per-frame provenance line in the manifest.
type FrameRecord struct {
	Name   string
	SHA256 string
	Track  int
	Seq    int64
	PTS    int64
	Rules  string
}

// Metrics is the acceptance summary printed at the end of a run.
type Metrics struct {
	Frames        int
	AVSyncMaxUs   float64
	LossRecovered int
	LossTotal     int
	P95Ms         int64
	ManifestSHA   string
}

// RecoveryRate is the fraction of lost packets rebuilt from parity.
// With no losses it is defined as 1.
func (m Metrics) RecoveryRate() float64 {
	if m.LossTotal == 0 {
		return 1
	}
	return float64(m.LossRecovered) / float64(m.LossTotal)
}

// Lines renders the metrics as key=value lines.
func (m Metrics) Lines() []string {
	return []string{
		fmt.Sprintf("frames=%d", m.Frames),
		fmt.Sprintf("av_sync_max_us=%.3f", m.AVSyncMaxUs),
		fmt.Sprintf("loss_recovery=%d/%d", m.LossRecovered, m.LossTotal),
		fmt.Sprintf("recovery_rate=%.3f", m.RecoveryRate()),
		fmt.Sprintf("p95_ms=%d", m.P95Ms),
		fmt.Sprintf("manifest_sha256=%s", m.ManifestSHA),
	}
}

// P95 returns the 95th percentile of the given latencies (nearest-rank).
func P95(lat []int64) int64 {
	if len(lat) == 0 {
		return 0
	}
	s := append([]int64(nil), lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (95*len(s) + 99) / 100 - 1
	return s[idx]
}

// Manifest builds the manifest body and its SHA-256 digest. The body
// records, for every frame, the source track, packet sequence, unified
// PTS and the metadata correction rules that were applied.
func Manifest(frames []FrameRecord) (body, digest string) {
	var b strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&b, "%s sha256=%s track=%d seq=%d pts_ns=%d rules=%s\n",
			f.Name, f.SHA256, f.Track, f.Seq, f.PTS, f.Rules)
	}
	body = b.String()
	sum := sha256.Sum256([]byte(body))
	return body, hex.EncodeToString(sum[:])
}
