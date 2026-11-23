package index

import (
	"testing"

	"archive/internal/manifest"
	"archive/internal/media"
	"archive/internal/timeline"
)

func repaired(t *testing.T, samples []manifest.RawSample) *timeline.RepairedTrack {
	t.Helper()
	tr := &manifest.RawTrack{ID: "v1", Type: "video", Timescale: 90000, Samples: samples}
	rt, err := timeline.Repair(tr, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func kf(dts, dur int64, key bool) manifest.RawSample {
	return manifest.RawSample{DTS: dts, Duration: dur, Size: 100, Keyframe: key}
}

func TestBuildByDecodeTime(t *testing.T) {
	rt := repaired(t, []manifest.RawSample{
		kf(9000, 3000, true), kf(12000, 3000, false), kf(15000, 3000, false),
		kf(18000, 3000, true), kf(21000, 3000, false), kf(24000, 3000, true),
	})
	b := NewBuilder(rt, 1<<20)
	off := uint64(0)
	for _, s := range rt.Samples {
		if err := b.Add(s, off); err != nil {
			t.Fatal(err)
		}
		off += uint64(s.Size)
	}
	es := b.Entries()
	if len(es) != 3 {
		t.Fatalf("entries=%d want 3", len(es))
	}
	// Decode-time order, program µs (media time, no edit list: the 100ms
	// non-zero start is preserved as program time).
	if es[0].DTSus != 100000 || es[1].DTSus != 200000 || es[2].DTSus != 266667 {
		t.Fatalf("DTSus: %+v", es)
	}
	// Preceding decode point chain.
	if es[0].PrevDTSus != -1 || es[1].PrevDTSus != 100000 || es[2].PrevDTSus != 200000 {
		t.Fatalf("PrevDTSus: %+v", es)
	}
	if es[1].ByteOffset != 300 {
		t.Fatalf("ByteOffset=%d want 300", es[1].ByteOffset)
	}
}

func TestTruncatedSampleNoOutOfRangeEntry(t *testing.T) {
	rt := repaired(t, []manifest.RawSample{
		kf(0, 3000, true), kf(3000, 3000, true), kf(6000, 3000, true),
	})
	b := NewBuilder(rt, 150) // media ends mid second sample
	if err := b.Add(rt.Samples[0], 0); err != nil {
		t.Fatal(err)
	}
	err := b.Add(rt.Samples[1], 100)
	me, ok := err.(*media.Error)
	if !ok {
		t.Fatalf("expected *media.Error, got %T", err)
	}
	if me.Track != "v1" || me.Offset != 100 {
		t.Fatalf("truncation error missing track/offset: %v", me)
	}
	if b.Add(rt.Samples[2], 200) == nil {
		t.Fatal("samples after truncation must fail")
	}
	if n := len(b.Entries()); n != 1 {
		t.Fatalf("entries=%d want 1 (no out-of-range entries)", n)
	}
	if media.ExitCode(err) != 2 {
		t.Fatal("truncation must map to exit code 2")
	}
}

func TestUnlocatableKeyframeSkipped(t *testing.T) {
	// Huge backward jump marks the interval unlocatable; a keyframe inside
	// it must not be fabricated into the index.
	rt := repaired(t, []manifest.RawSample{
		kf(0, 100, true), kf(100, 100, false), kf(5000, 100, false),
		kf(50, 100, true), kf(5200, 100, true),
	})
	b := NewBuilder(rt, 1<<20)
	off := uint64(0)
	for _, s := range rt.Samples {
		if err := b.Add(s, off); err != nil {
			t.Fatal(err)
		}
		off += 100
	}
	es := b.Entries()
	if len(es) != 2 || es[1].Seq != 4 {
		t.Fatalf("unlocatable keyframe leaked into index: %+v", es)
	}
}
