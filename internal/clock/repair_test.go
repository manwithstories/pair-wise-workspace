package clock

import (
	"math"
	"testing"

	"bcast/internal/boxes"
	"bcast/internal/fixture"
	"bcast/internal/media"
)

func TestUnwrapRTPAcrossUint32Boundary(t *testing.T) {
	const modulus = int64(1) << 32
	start := int64(math.MaxUint32) - 100
	wrapped := uint32((start + 5_000) % modulus)
	got := UnwrapRTP(start, wrapped, start+3_000)
	if got != start+5_000 {
		t.Fatalf("unwrapped=%d want %d", got, start+5_000)
	}
}

func TestDriftWrapVFRRepairIsMonotonicAndUsesAnchor(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.Generate("drift-wrap-vfr", dir); err != nil {
		t.Fatal(err)
	}
	scan, err := boxes.ScanBatch(dir, boxes.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	anchors, err := LoadAnchors(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Repair(scan, anchors, RepairOptions{})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	tt := res.Tracks[1]
	if math.Abs(tt.DriftPPM-5000) > 1 {
		t.Fatalf("drift=%.2f ppm, want about 5000", tt.DriftPPM)
	}

	var lastDTS int64 = -1
	foundAnchorRepair := false
	for _, ft := range tt.Fragments {
		for _, a := range ft.Actions {
			if a == "tfdt_regression_repaired_from_rtp" {
				foundAnchorRepair = true
			}
		}
		for _, s := range ft.Samples {
			if s.DTS < lastDTS {
				t.Fatalf("DTS regression at seq=%d index=%d: %d < %d", ft.Sequence, s.Index, s.DTS, lastDTS)
			}
			lastDTS = s.DTS
			if s.CompositionOffset == 0 && s.Index%4 != 0 {
				// Pattern has nonzero offsets on indexes 1,2,3.
				t.Fatalf("composition offsets were not preserved at index %d", s.Index)
			}
		}
	}
	if !foundAnchorRepair {
		t.Fatalf("expected RTP anchor repair action; actions=%v", allActions(tt))
	}
}

func TestMultiTrackSharedTimeBaseAndFailureBlocks(t *testing.T) {
	video := syntheticTrack(1, 90000, 3000, 3)
	audio := syntheticTrack(2, 48000, 1600, 3)
	rep := &boxes.BatchReport{Unique: []*boxes.Fragment{video, audio}}
	res, err := Repair(rep, nil, RepairOptions{AllowAnchorlessClamp: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(res.Tracks))
	}
	vEnd := res.Tracks[1].EndShared
	aEnd := res.Tracks[2].EndShared
	if math.Abs(float64(vEnd-aEnd)) > float64(media.SharedTimescale)/1000 {
		t.Fatalf("track ends differ too much: video=%d audio=%d", vEnd, aEnd)
	}

	bad := syntheticTrack(3, 90000, 3000, 2)
	bad.TFDT = 100
	bad.Sequence = 2
	bad.No = 2
	bad.MoofOffset = 777
	bad.Path = "seg-bad.m4s"
	first := syntheticTrack(3, 90000, 3000, 2)
	first.Sequence = 1
	blocked := &boxes.BatchReport{Unique: []*boxes.Fragment{first, bad}}
	_, err = Repair(blocked, nil, RepairOptions{AllowAnchorlessClamp: false})
	me, ok := media.AsError(err)
	if !ok || me.ExitCode != media.ExitCorrupt {
		t.Fatalf("err=%v want corrupt media error", err)
	}
	frag, off, _ := media.Origin(err)
	if frag != 2 || off != 777 {
		t.Fatalf("origin fragment=%d offset=%d, want 2/777", frag, off)
	}
}

func syntheticTrack(id, timescale, duration uint32, seq uint32) *boxes.Fragment {
	f := &boxes.Fragment{
		No:         int(seq),
		Path:       "synthetic.m4s",
		Sequence:   seq,
		TrackID:    id,
		Timescale:  timescale,
		MoofOffset: 0,
		TFDT:       uint64(seq-1) * uint64(duration) * 4,
		Samples:    make([]media.SampleTiming, 4),
	}
	for i := range f.Samples {
		f.Samples[i] = media.SampleTiming{Index: i, Duration: duration, Size: 32, Sync: i == 0}
	}
	return f
}

func allActions(tt *TrackTimeline) []string {
	var out []string
	for _, f := range tt.Fragments {
		out = append(out, f.Actions...)
	}
	return out
}
