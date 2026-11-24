// Package metrics validates each pipeline stage and the final output, and
// renders the machine-parseable key=value report used by operators and by the
// acceptance checks.
package metrics

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"relay/internal/media"
	"relay/internal/recovery"
	"relay/internal/txn"
)

// Thresholds advertised by the acceptance contract.
const (
	MaxSyncMS   = 2.0
	MinRecovery = 99.5
	MaxP95MS    = 100.0
)

// Collector accumulates timing observations during a run.
type Collector struct {
	batchMS []float64
}

// RecordBatch stores one batch processing duration.
func (c *Collector) RecordBatch(d time.Duration) {
	c.batchMS = append(c.batchMS, float64(d.Microseconds())/1000.0)
}

// P95BatchMS returns the 95th-percentile batch duration in milliseconds.
func (c *Collector) P95BatchMS() float64 {
	if len(c.batchMS) == 0 {
		return 0
	}
	sorted := append([]float64(nil), c.batchMS...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// Report is the full terminal output of one run.
type Report struct {
	Mode           string
	Samples        int
	SyncMaxMS      float64
	RecoveryPct    float64
	P95BatchMS     float64
	InputSHA       [32]byte
	OutputSHA      [32]byte
	ManifestSum    [32]byte
	CommitCount    int
	RollbackCount  int
	CurrentChanged bool
	Replayed       int
	Identical      bool
	ChecksumError  bool
	TrackDiffs     []TrackDiff
	Extra          map[string]string
}

// TrackDiff proves one remuxed track kept every encoded sample byte.
type TrackDiff struct {
	TrackID   uint32
	InputSHA  [32]byte
	OutputSHA [32]byte
	SampleN   int
	Preserved bool
}

// Validate checks the acceptance thresholds and the sample-preservation
// invariants; it returns the first violation as a structured media error.
func (r *Report) Validate() error {
	if r.Samples == 0 {
		return media.NewError(media.ErrMetadata, 0, "no samples processed")
	}
	if r.SyncMaxMS > MaxSyncMS {
		return media.NewError(media.ErrMetadata, 0,
			fmt.Sprintf("sync_max_ms %.3f exceeds %.1f", r.SyncMaxMS, MaxSyncMS))
	}
	if r.RecoveryPct < MinRecovery {
		return media.NewError(media.ErrGap, 0,
			fmt.Sprintf("recovery_pct %.2f below %.1f", r.RecoveryPct, MinRecovery))
	}
	if r.P95BatchMS > MaxP95MS {
		return media.NewError(media.ErrLimitExceeded, 0,
			fmt.Sprintf("p95_batch_ms %.3f exceeds %.1f", r.P95BatchMS, MaxP95MS))
	}
	if r.InputSHA != r.OutputSHA {
		return media.NewError(media.ErrChecksumMismatch, 0, "aggregate input/output digests differ")
	}
	for _, d := range r.TrackDiffs {
		if !d.Preserved {
			return media.NewError(media.ErrChecksumMismatch, d.TrackID, "remux altered sample bytes")
		}
	}
	return nil
}

// RecoveryPctOf converts a recovery result into delivered/expected percent.
// FEC-reconstructed packets count as delivered; remaining gaps do not.
func RecoveryPctOf(res *recovery.Result) float64 {
	if res == nil || res.ExpectedSpan == 0 {
		return 100
	}
	delivered := res.MediaCount
	return float64(delivered) * 100 / float64(res.ExpectedSpan)
}

// DigestSamples hashes the canonical ordering of a sample set. Canonical order
// is source, sequence; presentation order is intentionally irrelevant so that
// input and remuxed output can be compared on identical inputs.
func DigestSamples(frames []*media.RawFrame) [32]byte {
	ordered := append([]*media.RawFrame(nil), frames...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].Meta, ordered[j].Meta
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		return a.Sequence < b.Sequence
	})
	h := sha256.New()
	for _, fr := range ordered {
		var hdr [16]byte
		for i := 0; i < 4; i++ {
			hdr[i] = byte(fr.Meta.SourceID >> (24 - 8*i))
		}
		for i := 0; i < 8; i++ {
			hdr[4+i] = byte(fr.Meta.Sequence >> (56 - 8*i))
		}
		for i := 0; i < 4; i++ {
			hdr[12+i] = byte(fr.Meta.Size >> (24 - 8*i))
		}
		h.Write(hdr[:])
		h.Write(fr.Payload)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// CompareTracks builds per-track preservation verdicts between input frames
// and the frames re-read from the remuxed container.
func CompareTracks(in, out map[uint32][]*media.RawFrame) []TrackDiff {
	ids := make([]uint32, 0, len(in))
	for id := range in {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	diffs := make([]TrackDiff, 0, len(ids))
	for _, id := range ids {
		d := TrackDiff{TrackID: id, SampleN: len(in[id])}
		d.InputSHA = DigestSamples(in[id])
		if outFrames, ok := out[id]; ok {
			d.OutputSHA = DigestSamples(outFrames)
		}
		d.Preserved = d.InputSHA == d.OutputSHA && d.SampleN > 0
		diffs = append(diffs, d)
	}
	return diffs
}

// ManifestDigest exposes the deterministic manifest hash.
func ManifestDigest(m txn.Manifest) [32]byte { return txn.ManifestHash(m) }

// Render produces the stable key=value lines printed to the terminal.
func (r *Report) Render() string {
	var b strings.Builder
	add := func(k string, v interface{}) {
		fmt.Fprintf(&b, "%s=%v\n", k, v)
	}
	add("mode", r.Mode)
	add("samples", r.Samples)
	fmt.Fprintf(&b, "sync_max_ms=%.3f\n", r.SyncMaxMS)
	fmt.Fprintf(&b, "recovery_pct=%.2f\n", r.RecoveryPct)
	fmt.Fprintf(&b, "p95_batch_ms=%.3f\n", r.P95BatchMS)
	add("input_sha256", fmt.Sprintf("%x", r.InputSHA))
	add("output_sha256", fmt.Sprintf("%x", r.OutputSHA))
	add("manifest_sha256", fmt.Sprintf("%x", r.ManifestSum))
	add("commit", r.CommitCount)
	add("rollback", r.RollbackCount)
	add("current_unchanged", b2i(!r.CurrentChanged))
	add("replayed", r.Replayed)
	add("identical", r.Identical)
	add("checksum_mismatch", b2i(r.ChecksumError))
	for _, d := range r.TrackDiffs {
		fmt.Fprintf(&b, "track_%d_samples=%d preserved=%t\n", d.TrackID, d.SampleN, d.Preserved)
		fmt.Fprintf(&b, "track_%d_input_sha256=%x\n", d.TrackID, d.InputSHA)
		fmt.Fprintf(&b, "track_%d_output_sha256=%x\n", d.TrackID, d.OutputSHA)
	}
	keys := make([]string, 0, len(r.Extra))
	for k := range r.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, r.Extra[k])
	}
	return b.String()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
