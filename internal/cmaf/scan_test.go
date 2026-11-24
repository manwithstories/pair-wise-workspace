package cmaf

import (
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/gen"
)

func scanInit(t *testing.T) (*Scanner, InitSegment) {
	t.Helper()
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000, Width: 640, Height: 360})
	sc := NewScanner(DefaultLimits())
	seg, err := sc.ParseInit(mux.Init(), 0)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	sc.ApplyInit(seg)
	return sc, seg
}

func TestParseInitFields(t *testing.T) {
	_, seg := scanInit(t)
	if len(seg.Tracks) != 1 {
		t.Fatalf("tracks = %+v", seg.Tracks)
	}
	tr := seg.Tracks[0]
	if tr.TrackID != 1 || tr.Timescale != 90000 || tr.Codec != "avc1" ||
		tr.Handler != "vide" || tr.Width != 640 || tr.Height != 360 {
		t.Fatalf("track = %+v", tr)
	}
	if tr.DefaultSampleDuration != 3000 || tr.DefaultSampleSize != 512 {
		t.Fatalf("trex defaults = %+v", tr)
	}
}

func TestParseInitRejectsGarbage(t *testing.T) {
	sc := NewScanner(DefaultLimits())
	if _, err := sc.ParseInit([]byte("garbage-not-a-box"), 0); err == nil {
		t.Fatal("expected error for init without moov")
	}
}

func TestParseMediaReportsTfdtAndDurations(t *testing.T) {
	sc, _ := scanInit(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	segBytes, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 3, BaseTicks: 180000, Samples: 2})
	ms, err := sc.ParseMedia(segBytes, 4)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if ms.MoofCount != 1 || len(ms.Fragments) != 1 {
		t.Fatalf("media = %+v", ms)
	}
	fr := ms.Fragments[0]
	if fr.SequenceNumber != 3 || !fr.HasMfhd || !fr.HasTfdt {
		t.Fatalf("fragment header = %+v", fr)
	}
	if got := fr.TfdtTicks[1]; got != 180000 {
		t.Fatalf("tfdt = %d", got)
	}
	if len(fr.Samples) != 2 || fr.Samples[0].DurationTicks != 3000 {
		t.Fatalf("samples = %+v", fr.Samples)
	}
	if ms.MdatBytes == 0 {
		t.Fatal("mdat bytes not accounted")
	}
}

func TestParseMediaCorruptBoxStillFindsLaterMoof(t *testing.T) {
	sc, _ := scanInit(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	first, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 1, Samples: 1})
	second, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 2, BaseTicks: 3000, Samples: 1})
	// Corrupt the first mdat's declared size so the walker must resynchronize.
	corrupt := make([]byte, len(first))
	copy(corrupt, first)
	for off := 0; off+8 <= len(corrupt); {
		b, err := ParseBoxHeader(corrupt, off, len(corrupt))
		if err != nil || !b.Valid {
			break
		}
		if b.Type == "mdat" {
			corrupt[off], corrupt[off+1], corrupt[off+2], corrupt[off+3] = 0, 0, 0, 8
			break
		}
		off = b.End
	}
	combined := append(append([]byte{}, corrupt...), stripStypForTest(second)...)
	ms, err := sc.ParseMedia(combined, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(ms.Fragments) != 2 {
		t.Fatalf("fragments = %+v problems=%+v", ms.Fragments, ms.Problems)
	}
	if ms.Fragments[1].SequenceNumber != 2 {
		t.Fatalf("later moof not recovered: %+v", ms.Fragments)
	}
	var sawCorrupt bool
	for _, p := range ms.Problems {
		if p.Kind == clock.ErrBoxSizeOverrun || p.Kind == clock.ErrResync {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Fatalf("problems = %+v", ms.Problems)
	}
}

func stripStypForTest(seg []byte) []byte {
	b, err := ParseBoxHeader(seg, 0, len(seg))
	if err != nil || b.Type != "styp" {
		return seg
	}
	return seg[b.End:]
}

func TestParseMediaTruncatedLastBox(t *testing.T) {
	sc, _ := scanInit(t)
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	full, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 1, Samples: 4})
	ms, err := sc.ParseMedia(full[:len(full)-600], 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(ms.Fragments) != 1 {
		t.Fatalf("fragments = %+v", ms.Fragments)
	}
	var truncated int
	for _, sm := range ms.Fragments[0].Samples {
		if sm.Truncated {
			truncated++
		}
	}
	if truncated == 0 {
		t.Fatalf("expected truncated samples: %+v", ms.Fragments[0].Samples)
	}
}

func TestScannerLimits(t *testing.T) {
	sc := NewScanner(Limits{MaxSegmentBytes: 16, MaxBoxBytes: 16, MaxSamplesPerFragment: 2, MaxFragments: 1})
	if _, err := sc.ParseMedia(make([]byte, 1024), 1); err == nil {
		t.Fatal("expected segment limit error")
	}
	mux := gen.NewMuxer(gen.TrackSpec{TrackID: 1, Timescale: 90000})
	sc2 := NewScanner(Limits{MaxSegmentBytes: 1 << 20, MaxBoxBytes: 1 << 20, MaxSamplesPerFragment: 2, MaxFragments: 64})
	initSeg, err := sc2.ParseInit(mux.Init(), 0)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	sc2.ApplyInit(initSeg)
	seg, _ := mux.BuildFragment(gen.FragmentSpec{Sequence: 1, Samples: 4})
	ms, err := sc2.ParseMedia(seg, 1)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if len(ms.Fragments) != 1 {
		t.Fatalf("fragments = %+v", ms.Fragments)
	}
	if len(ms.Fragments[0].Samples) != 0 {
		t.Fatalf("trun over sample limit should be refused: %+v", ms.Fragments[0].Samples)
	}
}
