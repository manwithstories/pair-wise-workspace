package mp4

import (
	"bytes"
	"testing"

	"archivetl/internal/clock"
	"archivetl/internal/testdata"
)

func buildValid(t *testing.T) ([]byte, testdata.MP4Spec) {
	t.Helper()
	spec := testdata.MP4Spec{
		Timescale: 3000, SampleCount: 60, Delta: 100,
		SampleSize: 10, KeyEvery: 10, Chunks: 4,
	}
	data, err := testdata.BuildMP4(spec)
	if err != nil {
		t.Fatalf("fixture build: %v", err)
	}
	return data, spec
}

func TestBuildIndex(t *testing.T) {
	data, spec := buildValid(t)
	idx, derr := BuildIndex(bytes.NewReader(data), "clip.mp4")
	if derr != nil {
		t.Fatalf("BuildIndex: %v", derr)
	}
	if idx.Timescale != 3000 || len(idx.Samples) != int(spec.SampleCount) {
		t.Fatalf("index shape wrong: timescale=%d samples=%d", idx.Timescale, len(idx.Samples))
	}
	// 100 units at 3000 Hz = 1/30 s = 1000 ticks per sample.
	if idx.Samples[1].Ticks-idx.Samples[0].Ticks != 1000 {
		t.Fatalf("tick stride=%d want 1000", idx.Samples[1].Ticks-idx.Samples[0].Ticks)
	}
	if len(idx.Keyframes) != 6 || idx.Keyframes[0] != 1 || idx.Keyframes[5] != 51 {
		t.Fatalf("keyframes wrong: %v", idx.Keyframes)
	}
	// Offsets must stay inside the file and never overlap.
	for i, s := range idx.Samples {
		if s.Offset < 0 || s.Offset+int64(s.Size) > int64(len(data)) {
			t.Fatalf("sample %d out of file bounds", i)
		}
		if i > 0 && s.Offset < idx.Samples[i-1].Offset+int64(idx.Samples[i-1].Size) {
			t.Fatalf("sample %d overlaps previous sample", i)
		}
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("index/linear-scan divergence: %v", err)
	}
}

func TestKeyframeLookupSemantics(t *testing.T) {
	data, _ := buildValid(t)
	idx, derr := BuildIndex(bytes.NewReader(data), "clip.mp4")
	if derr != nil {
		t.Fatal(derr)
	}
	// Key samples are 1,11,21,... at ticks 0,10000,20000,...
	// Tick 15000 sits between key 11 (10000) and key 21 (20000).
	s, ok := idx.LookupKeyAt(clock.Tick(15000))
	if !ok || s.Number != 11 {
		t.Fatalf("lookup=%d ok=%v want sample 11", s.Number, ok)
	}
	// One tick before the next key still resolves to the previous one.
	s, ok = idx.LookupKeyAt(clock.Tick(19999))
	if !ok || s.Number != 11 {
		t.Fatalf("pre-boundary lookup=%d want 11", s.Number)
	}
	// Exactly on a keyframe returns that keyframe.
	s, ok = idx.LookupKeyAt(clock.Tick(20000))
	if !ok || s.Number != 21 {
		t.Fatalf("exact lookup=%d want 21", s.Number)
	}
	// The first sample is itself a keyframe at tick 0.
	if s0, ok := idx.LookupKeyAt(clock.Tick(0)); !ok || s0.Number != 1 {
		t.Fatalf("tick-0 lookup=%+v ok=%v want sample 1", s0, ok)
	}
	// Before any media: no hit, no panic.
	if _, ok := idx.LookupKeyAt(clock.Tick(-1)); ok {
		t.Fatal("negative tick must not match a keyframe")
	}
}

func TestCorruptFixtures(t *testing.T) {
	data, spec := buildValid(t)
	corrupt := testdata.BuildCorruptMP4(data, spec)
	if len(corrupt) < 3 {
		t.Fatalf("corrupt fixtures=%d", len(corrupt))
	}
	for _, c := range corrupt {
		t.Run(c.Name, func(t *testing.T) {
			idx, derr := BuildIndex(bytes.NewReader(c.Data), c.Name+".mp4")
			if derr == nil {
				t.Fatalf("%s accepted as valid, samples=%d", c.Name, len(idx.Samples))
			}
			if derr.Phase != "index" || derr.Source == "" {
				t.Fatalf("error missing phase/source: %v", derr)
			}
		})
	}
}

func TestDeterministicReindex(t *testing.T) {
	data, _ := buildValid(t)
	first, _ := BuildIndex(bytes.NewReader(data), "clip.mp4")
	second, _ := BuildIndex(bytes.NewReader(data), "clip.mp4")
	for i := range first.Samples {
		a, b := first.Samples[i], second.Samples[i]
		if a != b {
			t.Fatalf("sample %d differs between builds: %+v vs %+v", i, a, b)
		}
	}
}
