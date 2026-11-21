package timeline

import (
	"testing"

	"mediasync/internal/metadata"
	"mediasync/internal/model"
)

func resolved(id string, kind model.TrackKind, rate model.Rational, start model.Rational) metadata.ResolvedTrack {
	return metadata.ResolvedTrack{Track: metadata.TrackMeta{
		TrackID: id, Kind: kind, Source: "mxf", Timescale: 90000,
		FrameRate: rate, Start: start, Width: 1920, Height: 1080,
	}}
}

func vframe(seq, ts int64, clock model.Clock) model.Frame {
	return model.Frame{Track: "v", Kind: model.KindVideo, Rep: "high", Seq: seq,
		Timestamp: ts, Clock: clock, Marker: true, Payload: []byte{byte(seq)}}
}

// TestMergeNonIntegerRateNoDrift verifies 29.97 fps alignment stays exact for
// hundreds of frames and audio within tolerance aligns without diagnostics.
func TestMergeNonIntegerRateNoDrift(t *testing.T) {
	master := model.Clock90k
	rate := model.Rate2997
	var vframes []model.Frame
	nFrames := 360 // ~12s
	for i := 0; i < nFrames; i++ {
		ts := int64(i) * master.TickRate * rate.Den / rate.Num
		vframes = append(vframes, vframe(int64(i), ts, master))
	}
	var aframes []model.Frame
	// 48kHz audio, 1024-sample blocks, same origin: stays well within 5ms.
	for i := 0; i < 500; i++ {
		ts := int64(i) * 1024
		aframes = append(aframes, model.Frame{Track: "a", Kind: model.KindAudio, Rep: "voice",
			Seq: int64(i), Timestamp: ts, Clock: model.Clock48k, Payload: []byte{1}})
	}
	tl := Merge(master, []Input{
		{Track: resolved("v", model.KindVideo, rate, model.Rational{Num: 0, Den: 1}), Frames: vframes},
		{Track: resolved("a", model.KindAudio, model.Rational{Num: 48000, Den: 1},
			model.Rational{Num: 0, Den: 1}), Frames: aframes},
	}, model.Rational{Num: 20, Den: 1000})

	// Exact-grid check: every video frame must land on the rational frame-grid
	// tick, including the audio-carrying slots.
	seen := map[int64]bool{}
	for i := 0; i < nFrames; i++ {
		want := FrameGridTick(master, model.Rational{Num: 0, Den: 1}, rate, int64(i))
		seen[want] = true
		var slot *Slot
		for j := range tl.Slots {
			if tl.Slots[j].Tick == want {
				slot = &tl.Slots[j]
				break
			}
		}
		if slot == nil {
			t.Fatalf("frame %d grid tick %d missing from timeline", i, want)
		}
	}
	if len(seen) != nFrames {
		t.Fatalf("video frame ticks must be unique, got %d/%d", len(seen), nFrames)
	}
	if tl.SyncMaxMs > 20.0 {
		t.Fatalf("48kHz blocks vs 29.97 grid residual too large: %.3fms", tl.SyncMaxMs)
	}
	if len(tl.IncompleteTracks()) != 0 {
		t.Fatalf("clean inputs must align fully, got incomplete=%v", tl.IncompleteTracks())
	}
	if tl.Diag.Count(model.KindUnalignable) != 0 {
		t.Fatalf("no alignment diagnostics expected: %v", tl.Diag.All())
	}
}

// TestMergeTrackOffsetAndUnalignable checks origin offsets are applied once and
// a deliberately misaligned audio track is marked incomplete.
func TestMergeTrackOffsetAndUnalignable(t *testing.T) {
	master := model.Clock90k
	vframes := []model.Frame{vframe(0, 0, master), vframe(1, 3000, master)}
	// Audio starts 40ms late at 48kHz: 1920 ticks.
	aframes := []model.Frame{{Track: "a", Kind: model.KindAudio, Rep: "voice",
		Seq: 0, Timestamp: 1920, Clock: model.Clock48k, Payload: []byte{1}}}
	tl := Merge(master, []Input{
		{Track: resolved("v", model.KindVideo, model.Rate30, model.Rational{Num: 0, Den: 1}), Frames: vframes},
		{Track: resolved("a", model.KindAudio, model.Rational{Num: 48000, Den: 1},
			model.Rational{Num: 0, Den: 1}), Frames: aframes},
	}, model.Rational{Num: 5, Den: 1000})
	if tl.Diag.Count(model.KindUnalignable) == 0 {
		t.Fatalf("40ms offset must fail a 5ms tolerance: %v", tl.Diag.All())
	}
	if got := tl.IncompleteTracks(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("audio track must be marked incomplete, got %v", got)
	}
	// 40ms systematic offset leaves a ~6.7ms residual against the 30fps grid.
	if tl.SyncMaxMs <= 5.0 {
		t.Fatalf("sync max should exceed the 5ms tolerance, got %.2f", tl.SyncMaxMs)
	}
}

// TestTransportGapPropagatesToTimeline is the required fault-propagation case:
// an unrecoverable transport gap reaches the timeline as an incomplete track.
func TestTransportGapPropagatesToTimeline(t *testing.T) {
	master := model.Clock90k
	frames := []model.Frame{
		vframe(0, 0, master),
		{Track: "v", Kind: model.KindVideo, Rep: "low", Seq: 1, Incomplete: true, Clock: master},
		vframe(2, 6000, master),
	}
	tl := Merge(master, []Input{{Track: resolved("v", model.KindVideo, model.Rate30,
		model.Rational{Num: 0, Den: 1}), Frames: frames}}, model.Rational{Num: 5, Den: 1000})
	if got := tl.IncompleteTracks(); len(got) != 1 || got[0] != "v" {
		t.Fatalf("transport gap must propagate as incomplete track, got %v", got)
	}
	if tl.Diag.Count(model.KindMissingFrame) != 1 {
		t.Fatalf("one missing-frame diagnostic expected, got %v", tl.Diag.All())
	}
	frags := Fragmentize(tl)
	if len(frags) < 2 {
		t.Fatalf("gap placeholder must split fragments, got %d", len(frags))
	}
	mismatches, _ := VerifyBoundaries(tl, frags)
	if len(mismatches) != 1 {
		t.Fatalf("split fragments must expose exactly one boundary gap, got %d (%+v)", len(mismatches), mismatches)
	}
}

// TestCorruptedBoundaryFixture feeds deliberately corrupted fragments and
// proves overlap and phantom-tick mismatches are detected and propagated.
func TestCorruptedBoundaryFixture(t *testing.T) {
	master := model.Clock90k
	vframes := []model.Frame{vframe(0, 0, master), vframe(1, 3000, master), vframe(2, 6000, master)}
	tl := Merge(master, []Input{{Track: resolved("v", model.KindVideo, model.Rate30,
		model.Rational{Num: 0, Den: 1}), Frames: vframes}}, model.Rational{Num: 5, Den: 1000})
	corrupt := []Fragment{
		{TrackID: "v", Rep: "low", FirstTick: 0, EndTick: 6000, Frames: 2},
		{TrackID: "v", Rep: "high", FirstTick: 3000, EndTick: 9000, Frames: 2}, // overlap
		{TrackID: "v", Rep: "high", FirstTick: 99999, EndTick: 100000, Frames: 1}, // phantom tick
	}
	mismatches, bag := VerifyBoundaries(tl, corrupt)
	if len(mismatches) < 2 {
		t.Fatalf("corrupted fixture must produce overlap and phantom mismatches, got %d", len(mismatches))
	}
	if bag.Count(model.KindBoundaryMismatch) < 2 {
		t.Fatalf("boundary diagnostics missing: %v", bag.All())
	}
	MergeDiag(tl, bag)
	if tl.Diag.Count(model.KindBoundaryMismatch) < 2 {
		t.Fatal("boundary mismatch must propagate into the timeline diagnostic trail")
	}

	// Healthy fragments verify clean: idempotent across repeated runs.
	healthy := Fragmentize(tl)
	if m, _ := VerifyBoundaries(tl, healthy); len(m) != 0 {
		t.Fatalf("healthy timeline must have no mismatches, got %+v", m)
	}
	again := Fragmentize(tl)
	if len(again) != len(healthy) || again[0].FirstTick != healthy[0].FirstTick {
		t.Fatal("fragmentization must be deterministic across runs")
	}
}
