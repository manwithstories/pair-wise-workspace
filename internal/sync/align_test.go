package sync

import (
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
	"mediad/internal/transport"
)

func samples(n int, start, step model.Timestamp) []index.Sample {
	out := make([]index.Sample, n)
	for i := range out {
		out[i] = index.Sample{
			Index: uint32(i), Size: 10,
			PTS: start + model.Timestamp(i)*step,
			DTS: start + model.Timestamp(i)*step,
		}
	}
	return out
}

func TestAlignZeroDriftWhenTimelinesMatch(t *testing.T) {
	// both tracks share the same PTS grid; only the timescale metadata differs
	idx := &index.FileIndex{
		Name: "av.mp4",
		Tracks: []index.Track{
			{IsVideo: true, Timescale: 90000, Samples: samples(10, 0, 40*model.Millisecond)},
			{IsAudio: true, Timescale: 48000, Samples: samples(10, 0, 40*model.Millisecond)},
		},
	}
	rep := NewAligner().Align(idx, nil)
	if rep.AudioVideoDriftMS > 20 {
		t.Fatalf("av_drift_ms = %d exceeds 20ms budget", rep.AudioVideoDriftMS)
	}
	if len(rep.Tracks) != 2 || !rep.Tracks[0].IsVideo {
		t.Fatalf("alignment views wrong: %+v", rep.Tracks)
	}
}

func TestAlignCorrectsConstantOffset(t *testing.T) {
	idx := &index.FileIndex{
		Name: "offset.mp4",
		Tracks: []index.Track{
			{IsVideo: true, Timescale: 90000, Samples: samples(6, 100*model.Millisecond, 40*model.Millisecond)},
			{IsAudio: true, Timescale: 48000, Samples: samples(6, 0, 40*model.Millisecond)},
		},
	}
	rep := NewAligner().Align(idx, nil)
	// audio starts 100ms earlier than video; the aligner records that offset
	var audio *TrackView
	for i := range rep.Tracks {
		if rep.Tracks[i].IsAudio {
			audio = &rep.Tracks[i]
		}
	}
	if audio == nil {
		t.Fatal("audio view missing")
	}
	if audio.OffsetNS != 100*model.Millisecond {
		t.Fatalf("audio offset = %d, want 100ms", audio.OffsetNS)
	}
}

func TestAlignPropagatesTransportGapsAndIndexFaults(t *testing.T) {
	idx := &index.FileIndex{
		Name: "gap.mp4",
		Tracks: []index.Track{
			{IsVideo: true, Timescale: 90000, Samples: samples(4, 0, 33*model.Millisecond)},
		},
		Faults: []*model.Fault{model.NewFault(model.FaultOutOfBounds, 512, "gap.mp4", "bad sample")},
	}
	gaps := []transport.Gap{{StartSeq: 2, EndSeq: 3, PTS: 66000, Count: 2}}
	rep := NewAligner().Align(idx, gaps)
	sawGap, sawIndexFault := false, false
	for _, f := range rep.Faults {
		if f.Source != "gap.mp4" {
			t.Fatalf("fault source not propagated: %+v", f)
		}
		if f.Detail != "" && len(rep.Gaps) > 0 {
			sawGap = true
		}
		if f.Offset == 512 {
			sawIndexFault = true
		}
	}
	if !sawGap || !sawIndexFault {
		t.Fatalf("transport gap=%v index fault=%v must both reach sync", sawGap, sawIndexFault)
	}
}

func TestAlignNoTracksRecordsFault(t *testing.T) {
	idx := &index.FileIndex{Name: "empty.mp4"}
	rep := NewAligner().Align(idx, nil)
	if len(rep.Faults) == 0 || rep.Faults[0].Kind != model.FaultMissingIndex {
		t.Fatalf("expected missing_index fault, got %+v", rep.Faults)
	}
}

func TestSkewEstimation(t *testing.T) {
	// anchor spans 1000ms, track spans 1010ms -> 10000 ppm
	got := estimateSkew(1000*model.Millisecond, 1010*model.Millisecond)
	if got < 9990 || got > 10010 {
		t.Fatalf("skew ppm = %v, want ~10000", got)
	}
	if got := estimateSkew(0, 100); got != 0 {
		t.Fatalf("degenerate anchor span must yield 0 skew, got %v", got)
	}
}

func TestMultiCameraUsesFirstVideoAsAnchor(t *testing.T) {
	idx := &index.FileIndex{
		Name: "cams",
		Tracks: []index.Track{
			{Handler: "soun", IsAudio: true, Samples: samples(5, 0, 20*model.Millisecond)},
			{Handler: "vide", IsVideo: true, Samples: samples(5, 50*model.Millisecond, 40*model.Millisecond)},
			{Handler: "vide", IsVideo: true, Samples: samples(5, 0, 40*model.Millisecond)},
		},
	}
	rep := NewAligner().Align(idx, nil)
	// the first video track (index 1) is the anchor and must carry no offset
	if rep.Tracks[1].OffsetNS != 0 {
		t.Fatalf("anchor track offset = %d, want 0", rep.Tracks[1].OffsetNS)
	}
}
