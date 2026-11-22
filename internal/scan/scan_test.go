package scan

import (
	"testing"

	"avscan/internal/model"
)

func videoTrack() *model.Track {
	tr := &model.Track{ID: 1, Kind: model.KindVideo, Timescale: 24_000}
	// 24 samples at 1000 ticks each (~41.67ms); sync every 8 samples.
	for i := 0; i < 24; i++ {
		tr.Samples = append(tr.Samples, model.SampleEvent{
			TrackID: 1, Index: uint32(i),
			StartNS:    int64(i) * 41_666_666,
			DurationNS: 41_666_666,
			Offset:     uint64(1000 + i*100), Size: 100,
		})
	}
	for s := uint32(1); s <= 24; s += 8 {
		tr.SyncSamples = append(tr.SyncSamples, s)
	}
	return tr
}

func TestKeyFrameIntervals(t *testing.T) {
	r := AnalyzeKeyFrames(videoTrack())
	if r.MissingSync || r.SyncCount != 3 {
		t.Fatalf("sync count = %d missing=%v", r.SyncCount, r.MissingSync)
	}
	// Three syncs -> two equal gaps of 8 * 41.67ms.
	if len(r.GapNS) != 2 {
		t.Fatalf("gaps = %v", r.GapNS)
	}
	if r.MedianGapNS != r.MaxGapNS || r.MedianGapNS != 8*41_666_666 {
		t.Errorf("median=%d max=%d want %d", r.MedianGapNS, r.MaxGapNS, 8*41_666_666)
	}
}

func TestMissingSyncNotGuessed(t *testing.T) {
	tr := videoTrack()
	tr.SyncSamples = nil // no stss box
	r := AnalyzeKeyFrames(tr)
	if !r.MissingSync {
		t.Error("track without stss must be flagged missing sync")
	}
	if r.SyncCount != 0 || len(r.GapNS) != 0 {
		t.Error("must not invent key-frame boundaries")
	}
}

func TestLocateDamageSnapsToSamples(t *testing.T) {
	tr := videoTrack()
	tr.ParseFaults = []model.Fault{
		model.NewSampleFault(model.FaultOffsetOutOfRange, "f.mp4", 1, 5, &tr.Samples[5], "bad"),
		model.NewSampleFault(model.FaultInvalidLength, "f.mp4", 1, 6, &tr.Samples[6], "bad"),
		// Far-away fault opens a second segment.
		model.NewSampleFault(model.FaultOffsetOutOfRange, "f.mp4", 1, 20, &tr.Samples[20], "bad"),
	}
	tl := &model.Timeline{Tracks: []*model.Track{tr}}
	res := LocateDamage(tl, nil)
	if len(res.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(res.Segments))
	}
	first := res.Segments[0]
	// Bounds must equal the sample boundaries of samples 5..6.
	if first.StartOffset != int64(tr.Samples[5].Offset) {
		t.Errorf("start = %d, want %d", first.StartOffset, tr.Samples[5].Offset)
	}
	wantEnd := int64(tr.Samples[6].Offset) + int64(tr.Samples[6].Size)
	if first.EndOffset != wantEnd {
		t.Errorf("end = %d, want %d", first.EndOffset, wantEnd)
	}
	if first.FaultCount != 2 || len(first.Kinds) != 2 {
		t.Errorf("merged segment counts wrong: %+v", first)
	}
	if res.FirstBadOffset != int64(tr.Samples[5].Offset) {
		t.Errorf("first bad offset = %d", res.FirstBadOffset)
	}
}

func TestLocateDamageGlobalBoxFault(t *testing.T) {
	tl := &model.Timeline{Tracks: []*model.Track{videoTrack()}}
	globals := []model.Fault{
		model.NewByteFault(model.FaultTruncatedBox, "f.mp4", 777, "cut"),
	}
	res := LocateDamage(tl, globals)
	if res.FirstBadOffset != 777 {
		t.Errorf("first bad offset = %d, want 777", res.FirstBadOffset)
	}
	if len(res.Segments) != 1 || res.Segments[0].StartOffset != 777 {
		t.Errorf("box-level segment wrong: %+v", res.Segments)
	}
}

func TestLocateDamageWithinOneSampleTolerance(t *testing.T) {
	tr := videoTrack()
	tr.ParseFaults = []model.Fault{
		model.NewSampleFault(model.FaultOffsetOutOfRange, "f.mp4", 1, 10, &tr.Samples[10], "a"),
		model.NewSampleFault(model.FaultOffsetOutOfRange, "f.mp4", 1, 12, &tr.Samples[12], "b"),
	}
	res := LocateDamage(&model.Timeline{Tracks: []*model.Track{tr}}, nil)
	if len(res.Segments) != 1 {
		t.Fatalf("faults two samples apart must merge, got %d", len(res.Segments))
	}
}
