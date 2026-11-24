package fixture

import (
	"errors"
	"testing"

	"mediatime/internal/manifest"
	"mediatime/internal/segment"
)

func TestGenerateNormalRoundTripsThroughSegment(t *testing.T) {
	gen := NewGenerator(41)
	sc, err := gen.Generate(24, FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.FragmentSpec) != 24 {
		t.Fatalf("fragments = %d", len(sc.FragmentSpec))
	}
	for _, spec := range sc.FragmentSpec {
		data := BuildFragmentBytes(spec)
		f, err := segment.ExtractFragment(spec.Sequence, spec.URI, data)
		if err != nil {
			t.Fatalf("fragment %d extract: %v", spec.Sequence, err)
		}
		if len(f.Samples) != samplesPerFragment {
			t.Fatalf("fragment %d samples = %d", spec.Sequence, len(f.Samples))
		}
	}
	// 配套清单必须能被 manifest 包解析且片段数一致。
	m, err := manifest.ParseHLS([]byte(sc.HLSPlaylist))
	if err != nil {
		t.Fatalf("playlist parse: %v", err)
	}
	if len(m.Fragments) != 24 {
		t.Fatalf("playlist fragments = %d", len(m.Fragments))
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a, _ := NewGenerator(41).Generate(24, FaultNone)
	b, _ := NewGenerator(41).Generate(24, FaultNone)
	for i := range a.FragmentSpec {
		da := BuildFragmentBytes(a.FragmentSpec[i])
		db := BuildFragmentBytes(b.FragmentSpec[i])
		if len(da) != len(db) {
			t.Fatalf("fragment %d size differs across runs", i)
		}
		for j := range da {
			if da[j] != db[j] {
				t.Fatalf("fragment %d byte %d differs", i, j)
			}
		}
	}
}

func TestWrapDriftVFRHasWrapAndDuplicate(t *testing.T) {
	sc, err := NewGenerator(41).Generate(24, FaultWrapDriftVFR)
	if err != nil {
		t.Fatal(err)
	}
	// 至少有一个分片的首样本 DTS 小于前片（发生回绕）。
	wraps := 0
	var prev uint64
	for i, spec := range sc.FragmentSpec {
		first := spec.Samples[0].DTS
		if i > 0 && first < prev {
			wraps++
		}
		prev = first
		// VFR 重复：第 5 片第 7、8 样本 DTS 相同。
		if spec.Sequence == 5 {
			if spec.Samples[7].DTS != spec.Samples[6].DTS {
				t.Fatal("expected injected duplicate DTS")
			}
		}
	}
	if wraps != 1 {
		t.Fatalf("wrap count = %d, want 1", wraps)
	}
}

func TestMissingScenarioSkipsFragments(t *testing.T) {
	sc, err := NewGenerator(41).Generate(24, FaultMissing)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.FragmentSpec) != 22 {
		t.Fatalf("expected 22 present fragments, got %d", len(sc.FragmentSpec))
	}
	for _, spec := range sc.FragmentSpec {
		if spec.Sequence == 8 || spec.Sequence == 9 {
			t.Fatal("missing fragment must not be generated")
		}
	}
}

func TestTruncatedFragmentRejected(t *testing.T) {
	sc, err := NewGenerator(41).Generate(24, FaultTruncated)
	if err != nil {
		t.Fatal(err)
	}
	spec := sc.FragmentSpec[3]
	if spec.TruncateAt == 0 {
		t.Fatal("truncation not configured")
	}
	data := BuildFragmentBytes(spec)
	if _, err := segment.ExtractFragment(spec.Sequence, spec.URI, data);
		!errors.Is(err, segment.ErrTruncatedBox) && !errors.Is(err, segment.ErrBoxOverflow) {
		t.Fatalf("want truncated/overflow error, got %v", err)
	}
}

func TestConflictPlaylistRejected(t *testing.T) {
	sc, err := NewGenerator(41).Generate(24, FaultConflict)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.ParseHLS([]byte(sc.ConflictPlaylist)); !errors.Is(err, manifest.ErrDuplicateSequence) {
		t.Fatalf("want ErrDuplicateSequence, got %v", err)
	}
}
