package gen

import (
	"fmt"
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

func TestRoundTripCleanSegment(t *testing.T) {
	mux := NewMuxer(TrackSpec{TrackID: 1, Timescale: 90000, Width: 1280, Height: 720})
	initBytes := mux.Init()
	sc := cmaf.NewScanner(cmaf.DefaultLimits())
	initSeg, err := sc.ParseInit(initBytes, 0)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if len(initSeg.Tracks) != 1 {
		t.Fatalf("tracks = %+v", initSeg.Tracks)
	}
	tr := initSeg.Tracks[0]
	if tr.TrackID != 1 || tr.Timescale != 90000 || tr.Handler != "vide" || tr.Codec != "avc1" {
		t.Fatalf("track = %+v", tr)
	}
	if tr.Width != 1280 || tr.Height != 720 {
		t.Fatalf("dimensions = %dx%d", tr.Width, tr.Height)
	}
	if tr.DefaultSampleDuration != 3000 {
		t.Fatalf("trex duration = %d", tr.DefaultSampleDuration)
	}
	sc.ApplyInit(initSeg)
	if len(initSeg.Problems) != 0 {
		t.Fatalf("init problems = %+v", initSeg.Problems)
	}

	keyframes := []int{0, 2}
	segBytes, info := mux.BuildFragment(FragmentSpec{Sequence: 1, BaseTicks: 0, Samples: 3, Keyframes: keyframes})
	media, err := sc.ParseMedia(segBytes, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(media.Problems) != 0 {
		t.Fatalf("media problems = %+v", media.Problems)
	}
	if len(media.Fragments) != 1 {
		t.Fatalf("fragments = %+v", media.Fragments)
	}
	frag := media.Fragments[0]
	if frag.SequenceNumber != info.Sequence || !frag.HasMfhd || !frag.HasTfdt {
		t.Fatalf("frag header = %+v", frag)
	}
	if len(frag.Samples) != 3 {
		t.Fatalf("samples = %+v", frag.Samples)
	}
	for i, sm := range frag.Samples {
		if sm.Truncated {
			t.Fatalf("sample %d unexpectedly truncated", i)
		}
		if sm.DurationTicks != 3000 || sm.Size != 512 {
			t.Fatalf("sample %d = %+v", i, sm)
		}
		if sm.DecodeTimeTicks != uint64(i*3000) {
			t.Fatalf("sample %d dts = %d", i, sm.DecodeTimeTicks)
		}
		wantKey := keyframes[0] == i || keyframes[1] == i
		if sm.IsKeyframe != wantKey {
			t.Fatalf("sample %d keyframe = %v, want %v", i, sm.IsKeyframe, wantKey)
		}
	}
}

func TestTruncatedSegmentMarksSamples(t *testing.T) {
	mux := NewMuxer(TrackSpec{TrackID: 1, Timescale: 90000})
	sc := cmaf.NewScanner(cmaf.DefaultLimits())
	initSeg, err := sc.ParseInit(mux.Init(), 0)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	sc.ApplyInit(initSeg)

	full, _ := mux.BuildFragment(FragmentSpec{Sequence: 1, Samples: 4})
	cut := len(full) - 700
	trunc, _ := mux.BuildFragment(FragmentSpec{Sequence: 1, Samples: 4, TruncateTo: cut})
	media, err := sc.ParseMedia(trunc, 2)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(media.Fragments) != 1 {
		t.Fatalf("fragments = %+v problems=%+v", media.Fragments, media.Problems)
	}
	frag := media.Fragments[0]
	if len(frag.Samples) != 4 {
		t.Fatalf("samples = %+v", frag.Samples)
	}
	var truncated int
	for _, sm := range frag.Samples {
		if sm.Truncated {
			truncated++
		}
	}
	if truncated == 0 {
		t.Fatal("expected at least one truncated sample")
	}
	var sawSegment bool
	for _, p := range media.Problems {
		if p.Kind == clock.ErrSegmentTruncated {
			sawSegment = true
		}
	}
	if !sawSegment {
		t.Fatalf("problems = %+v", media.Problems)
	}
}

func TestDropMfhdRecorded(t *testing.T) {
	mux := NewMuxer(TrackSpec{TrackID: 1})
	sc := cmaf.NewScanner(cmaf.DefaultLimits())
	initSeg, _ := sc.ParseInit(mux.Init(), 0)
	sc.ApplyInit(initSeg)
	seg, _ := mux.BuildFragment(FragmentSpec{Sequence: 1, Samples: 2, DropMfhd: true})
	media, err := sc.ParseMedia(seg, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(media.Fragments) != 1 {
		t.Fatalf("fragments = %+v", media.Fragments)
	}
	if media.Fragments[0].HasMfhd {
		t.Fatal("expected missing mfhd to be reported")
	}
	if len(media.Fragments[0].Problems) == 0 {
		t.Fatal("expected a problem for missing mfhd")
	}
}

func TestSegmentLimitRejected(t *testing.T) {
	mux := NewMuxer(TrackSpec{TrackID: 1})
	sc := cmaf.NewScanner(cmaf.Limits{MaxSegmentBytes: 64, MaxBoxBytes: 1 << 20})
	initSeg, _ := sc.ParseInit(mux.Init(), 0)
	sc.ApplyInit(initSeg)
	seg, _ := mux.BuildFragment(FragmentSpec{Sequence: 1, Samples: 8})
	if _, err := sc.ParseMedia(seg, 1); err == nil {
		t.Fatal("expected limit error")
	}
}

func TestMultipleFragmentsInOneSegment(t *testing.T) {
	mux := NewMuxer(TrackSpec{TrackID: 1})
	sc := cmaf.NewScanner(cmaf.DefaultLimits())
	initSeg, _ := sc.ParseInit(mux.Init(), 0)
	sc.ApplyInit(initSeg)
	a, _ := mux.BuildFragment(FragmentSpec{Sequence: 1, Samples: 2})
	b, _ := mux.BuildFragment(FragmentSpec{Sequence: 2, BaseTicks: 6000, Samples: 2})
	media, err := sc.ParseMedia(append(a[0:0:0], append(stripStyp(a), stripStyp(b)...)...), 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(media.Fragments) != 2 {
		t.Fatalf("fragments = %d", len(media.Fragments))
	}
	if media.Fragments[0].SequenceNumber != 1 || media.Fragments[1].SequenceNumber != 2 {
		t.Fatalf("sequence order = %+v", media.Fragments)
	}
	second := media.Fragments[1]
	for _, sm := range second.Samples {
		if sm.DecodeTimeTicks < 6000 {
			t.Fatalf("second fragment dts = %d", sm.DecodeTimeTicks)
		}
	}
}

// stripStyp removes a leading styp box so fragments can be concatenated.
func stripStyp(seg []byte) []byte {
	hdr, err := cmaf.ParseBoxHeader(seg, 0, len(seg))
	if err != nil || hdr.Type != "styp" {
		return seg
	}
	return seg[hdr.End:]
}

func BenchmarkParse30sThreeReps(b *testing.B) {
	mux := NewMuxer(TrackSpec{TrackID: 1})
	initSeg, _ := func() (cmaf.InitSegment, error) {
		sc := cmaf.NewScanner(cmaf.DefaultLimits())
		is, err := sc.ParseInit(mux.Init(), 0)
		return is, err
	}()
	var segs [][]byte
	for i := 1; i <= 30; i++ {
		seg, _ := mux.BuildFragment(FragmentSpec{Sequence: uint32(i), BaseTicks: uint64(i-1) * 90000, Samples: 30})
		segs = append(segs, seg)
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		sc := cmaf.NewScanner(cmaf.DefaultLimits())
		sc.ApplyInit(initSeg)
		for i, seg := range segs {
			if _, err := sc.ParseMedia(seg, i+1); err != nil {
				b.Fatal(err)
			}
		}
	}
}

var _ = fmt.Sprintf
