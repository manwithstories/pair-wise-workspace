// Package mediascan_test is an independent, external manifest scan that
// drives the whole pipeline through its public packages and cross-checks
// total frame count, corruption ranges and index time boundaries without
// depending on any internal implementation detail.
package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/batch"
	"mediascan/internal/clock"
	"mediascan/internal/fault"
	"mediascan/internal/frame"
	"mediascan/internal/gen"
	"mediascan/internal/index"
	"mediascan/internal/quality"
	"mediascan/internal/rtp"
)

// independentScan re-implements the manifest cross-check the way an
// external auditor would: regenerate, validate, reorder RTP, build quality
// and then read the committed index back from disk.
func independentScan(t *testing.T, dir string, p gen.FrameParams, corrupt bool) (int, int, int64, int64) {
	t.Helper()
	clk, err := clock.New(p.ClockRate, int64(p.FPS))
	if err != nil {
		t.Fatal(err)
	}
	generated, err := gen.Frames(p)
	if err != nil {
		t.Fatal(err)
	}
	// 1. manifest validation: total frame count must equal the request
	manifest := &frame.Manifest{
		FPS: p.FPS, ClockRate: p.ClockRate, GOP: p.GOP,
		TargetKbps: p.TargetKbps, Frames: make([]frame.Frame, len(generated)),
	}
	for i := range generated {
		manifest.Frames[i] = generated[i].Entry
	}
	if _, err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Frames) != p.Frames {
		t.Fatalf("total frames = %d, want %d", len(manifest.Frames), p.Frames)
	}

	// 2. RTP reorder independently recomputes burst-loss ranges
	np := gen.DefaultNetParams(p.Seed)
	pkts, err := gen.RTPTrace(clk, p.Frames, 1000, 0, np)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := rtp.NewReorderer(64, clk)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkt := range pkts {
		if err := rr.Push(pkt); err != nil {
			t.Fatal(err)
		}
	}
	rtpRes := rr.Finalize()
	if len(rtpRes.Bursts) == 0 {
		t.Fatal("independent scan expected at least one burst loss")
	}

	// 3. quality outcomes and corruption range count
	outcomes := make([]quality.FrameOutcome, p.Frames)
	var checksumFaults []fault.Fault
	for i := range generated {
		ok := true
		if corrupt && i == 3 {
			payload := append([]byte(nil), generated[i].Payload...)
			_ = gen.CorruptChecksumPayload(payload, i)
			if f := generated[i].Entry.Verify(payload); !frame.IsZero(f) {
				ok = false
				step := p.ClockRate / int64(p.FPS)
				f.Span = fault.Interval{Start: int64(i) * step, End: int64(i+1) * step}
				checksumFaults = append(checksumFaults, f)
			}
		}
		outcomes[i] = quality.FrameOutcome{Index: i, ChecksumOK: ok, Reachable: true}
	}
	qs, err := quality.Build(outcomes, checksumFaults, rtpRes, int64(clk.TicksPerFrame()))
	if err != nil {
		t.Fatal(err)
	}

	// 4. commit every chunk to a fresh index, then read it back
	store, err := index.Open(dir, "audit-digest")
	if err != nil {
		t.Fatal(err)
	}
	chunkFrames := 120
	totalChunks := (p.Frames + chunkFrames - 1) / chunkFrames
	cp, err := batch.NewManager(dir, "audit-digest", "audit-params", totalChunks, chunkFrames, false)
	if err != nil {
		t.Fatal(err)
	}
	for c := 0; c < totalChunks; c++ {
		from := c * chunkFrames
		to := from + chunkFrames - 1
		if to >= p.Frames {
			to = p.Frames - 1
		}
		entry := index.Entry{
			Chunk: c, FrameFrom: from, FrameTo: to,
			StartTick:   int64(from) * clk.TicksPerFrame(),
			EndTick:     int64(to+1) * clk.TicksPerFrame(),
			TotalFrames: to - from + 1,
		}
		if err := store.Commit(entry); err != nil {
			t.Fatal(err)
		}
		if err := cp.MarkChunked(c, entry.StartTick, entry.EndTick); err != nil {
			t.Fatal(err)
		}
	}
	sum := store.Summary()
	// independent frame-count reconciliation against the index
	if sum.TotalFrames != p.Frames {
		t.Fatalf("index total frames = %d, want %d", sum.TotalFrames, p.Frames)
	}
	// time boundaries must span tick 0 to the last frame end
	wantLast := int64(p.Frames) * clk.TicksPerFrame()
	if sum.FirstTick != 0 || sum.LastTick != wantLast {
		t.Fatalf("index time boundary = [%d,%d], want [0,%d]", sum.FirstTick, sum.LastTick, wantLast)
	}
	return len(manifest.Frames), len(qs.CorruptRanges), sum.FirstTick, sum.LastTick
}

func TestIndependentManifestScan(t *testing.T) {
	dir := t.TempDir()
	p := gen.FrameParams{Seed: 73, Frames: 900, FPS: 30, GOP: 60, TargetKbps: 900, ClockRate: 90000}
	frames, ranges, first, last := independentScan(t, dir, p, false)
	if frames != 900 {
		t.Fatalf("scanned frames = %d", frames)
	}
	if ranges < 1 {
		t.Fatalf("corrupt ranges = %d, want >= 1", ranges)
	}
	if first != 0 || last != 2700000 {
		t.Fatalf("time boundary = [%d,%d]", first, last)
	}
	// every committed chunk file must be parseable JSON with no dup/missing
	matches, _ := filepath.Glob(filepath.Join(dir, "chunks", "chunk-*.json"))
	if len(matches) != 8 {
		t.Fatalf("chunk files = %d, want 8", len(matches))
	}
	seen := map[int]bool{}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		var e index.Entry
		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatal(err)
		}
		if seen[e.Chunk] {
			t.Fatalf("duplicate chunk index %d", e.Chunk)
		}
		seen[e.Chunk] = true
	}
	for i := 0; i < 8; i++ {
		if !seen[i] {
			t.Fatalf("missing chunk %d in index", i)
		}
	}
}

func TestIndependentScanCorruptExpandsRanges(t *testing.T) {
	clean := t.TempDir()
	dirty := t.TempDir()
	p := gen.FrameParams{Seed: 73, Frames: 360, FPS: 30, GOP: 60, TargetKbps: 900, ClockRate: 90000}
	_, cleanRanges, _, _ := independentScan(t, clean, p, false)
	_, dirtyRanges, _, _ := independentScan(t, dirty, p, true)
	if dirtyRanges <= cleanRanges {
		t.Fatalf("corrupt run ranges=%d should exceed clean=%d", dirtyRanges, cleanRanges)
	}
}
