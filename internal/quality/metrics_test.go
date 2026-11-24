package quality

import (
	"testing"

	"mediac/internal/core"
	"mediac/internal/inspect"
	"mediac/internal/manifest"
	"mediac/internal/transport"
)

func buildTestManifest() *manifest.Manifest {
	m := &manifest.Manifest{Kind: manifest.KindDASH}
	targets := []int64{400000, 800000, 1600000}
	for i, bps := range targets {
		rep := manifest.Representation{
			ID: repID(i), Codec: "avc1.640028", TargetBPS: bps, Timescale: 1000,
		}
		for s := 0; s < 24; s++ {
			rep.Segments = append(rep.Segments, manifest.Segment{
				Index: s + 1, StartMs: int64(s) * 1000, DurationMs: 1000, Line: s + 2,
			})
		}
		m.Representations = append(m.Representations, rep)
	}
	return m
}

func repID(i int) string {
	return []string{"v-low", "v-mid", "v-high"}[i]
}

// cleanReports builds 24 structurally sound fragments with 4 samples each,
// one keyframe per fragment, sample sizes calibrated to hit the target bps.
func cleanReports(m *manifest.Manifest, repIndex int) []*inspect.FragmentReport {
	rep := m.Representations[repIndex]
	var out []*inspect.FragmentReport
	// Calibrate sample size so 24 fragments * 4 samples realize the target
	// bitrate over the 24 s window: totalBytes = targetBps * 24 / 8.
	totalBytes := rep.TargetBPS * 24 / 8
	sampleSize := uint32(totalBytes / (24 * 4))
	for f := 0; f < 24; f++ {
		fr := &inspect.FragmentReport{
			RepID: rep.ID, Fragment: f + 1, Timescale: 1000,
			BaseDecodeT: uint64(f * 4000),
		}
		for s := 0; s < 4; s++ {
			key := s == 0
			fr.Samples = append(fr.Samples, inspect.SampleInfo{
				Index: s, Size: sampleSize, Duration: 1000, IsKeyframe: key,
			})
		}
		out = append(out, fr)
	}
	return out
}

func TestComputeCleanMetrics(t *testing.T) {
	m := buildTestManifest()
	in := Input{
		Manifest: m, RepIndex: 1,
		Reports:   cleanReports(m, 1),
		Recovered: allCompleteRecovered(24),
	}
	got := Compute(in)
	if got.ValidSamplePct != 100 {
		t.Fatalf("valid sample pct=%.2f want 100", got.ValidSamplePct)
	}
	if got.DamagedSegments != 0 || len(got.DamagedRanges) != 0 {
		t.Fatalf("clean sample must have no damage: %+v", got.DamagedRanges)
	}
	if mathAbs(got.BitrateDeltaPct) > 5 {
		t.Fatalf("bitrate delta %.2f%% exceeds 5%%", got.BitrateDeltaPct)
	}
	if got.AlignmentMaxMs != 0 {
		t.Fatalf("aligned manifests must have zero drift, got %d", got.AlignmentMaxMs)
	}
	// One keyframe per fragment; consecutive keyframes are 4000 ticks apart on
	// the 1000-time-scale track, i.e. a 4000 ms GOP.
	if got.GOPQuantilesMs[95] != 4000 {
		t.Fatalf("gop p95=%.2f want 4000ms", got.GOPQuantilesMs[95])
	}
	if got.TotalSamples != 96 || got.ValidSamples != 96 {
		t.Fatalf("sample counts wrong: %d/%d", got.ValidSamples, got.TotalSamples)
	}
}

func TestComputeDamagedRanges(t *testing.T) {
	m := buildTestManifest()
	reports := cleanReports(m, 0)
	// Fragment 5 truncated, fragment 9 has invalid NAL.
	reports[4].Truncated = true
	reports[8].HasInvalidNAL = true
	recovered := allCompleteRecovered(24)
	// Add an unrecovered burst loss on fragment 13.
	recovered[12].Complete = false
	recovered[12].Gaps = []transport.ByteGap{
		{Start: 0, End: 100, FirstMissing: 20, LastMissing: 23},
	}
	faults := []core.Fault{
		core.NewFault(core.FaultBurstLoss, core.SevDamaged, "gap",
			core.Source{RepID: repID(0), SegmentIndex: 13}),
	}
	in := Input{Manifest: m, RepIndex: 0, Reports: reports,
		Recovered: recovered, Faults: faults}
	got := Compute(in)
	if got.DamagedSegments < 3 {
		t.Fatalf("damaged segments=%d want >=3", got.DamagedSegments)
	}
	if len(got.DamagedRanges) < 3 {
		t.Fatalf("damaged ranges=%d want >=3", len(got.DamagedRanges))
	}
	// Ranges must land on the manifest timeline with real source locations.
	for _, r := range got.DamagedRanges {
		if r.Source.SegmentIndex == 0 || r.EndMs <= r.StartMs {
			t.Fatalf("range missing source/time: %+v", r)
		}
	}
	if got.ValidSamplePct == 100 {
		t.Fatal("damaged fragments must reduce valid sample ratio")
	}
}

func TestAlignmentErrorDetectsDrift(t *testing.T) {
	m := buildTestManifest()
	// Shift the third representation's segment starts by 12 ms.
	for i := range m.Representations[2].Segments {
		m.Representations[2].Segments[i].StartMs += 12
	}
	in := Input{Manifest: m, RepIndex: 0, Reports: cleanReports(m, 0),
		Recovered: allCompleteRecovered(24)}
	got := Compute(in)
	if got.AlignmentMaxMs != 12 {
		t.Fatalf("alignment max=%d want 12", got.AlignmentMaxMs)
	}
}

func TestWarningsDoNotDamage(t *testing.T) {
	m := buildTestManifest()
	faults := []core.Fault{
		core.NewFault(core.FaultDuplicatePacket, core.SevWarn, "dup",
			core.Source{RepID: repID(1), SegmentIndex: 4}),
		core.NewFault(core.FaultReorderedPacket, core.SevWarn, "reorder",
			core.Source{RepID: repID(1), SegmentIndex: 4}),
	}
	in := Input{Manifest: m, RepIndex: 1, Reports: cleanReports(m, 1),
		Recovered: allCompleteRecovered(24), Faults: faults}
	got := Compute(in)
	if got.DamagedSegments != 0 {
		t.Fatalf("warn-level faults must not damage ranges: %+v", got.DamagedRanges)
	}
}

func allCompleteRecovered(n int) []transport.FragmentResult {
	out := make([]transport.FragmentResult, n)
	for i := range out {
		out[i] = transport.FragmentResult{RepID: "x", Fragment: i + 1, Complete: true}
	}
	return out
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
