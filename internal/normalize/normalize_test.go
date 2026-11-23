package normalize

import (
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
	"archivecheck/internal/isobmff"
)

func TestAudioSupportedLayouts(t *testing.T) {
	for ch, want := range map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"} {
		d, fl := Audio(1, ch, 48000)
		if fl != nil {
			t.Fatalf("%dch: unexpected fault %v", ch, fl)
		}
		if d.Layout != want {
			t.Fatalf("%dch: layout = %q, want %q", ch, d.Layout, want)
		}
	}
}

func TestAudioUnsupportedBlocks(t *testing.T) {
	for _, tc := range []struct{ ch, rate int }{{3, 48000}, {2, 12345}, {0, 48000}} {
		d, fl := Audio(2, tc.ch, tc.rate)
		if d != nil {
			t.Fatalf("ch=%d rate=%d: must not produce a descriptor", tc.ch, tc.rate)
		}
		if fl == nil || fl.Kind != fault.KindUnsupportedLayout {
			t.Fatalf("ch=%d rate=%d: fault = %v", tc.ch, tc.rate, fl)
		}
		if fl.Recoverable || fl.FirstBlocked() != "normalize" {
			t.Fatalf("fault must block at normalize: %+v", fl)
		}
	}
}

func colorOf(c fixture.Color) *isobmff.ColorInfo {
	return &isobmff.ColorInfo{Primaries: c.Primaries, Transfer: c.Transfer,
		Matrix: c.Matrix, FullRange: c.FullRange}
}

func TestColorConflictKeepSource(t *testing.T) {
	reps := []string{"800k", "1800k", "3500k"}
	srcs := []*isobmff.ColorInfo{colorOf(fixture.ColorBT709), colorOf(fixture.ColorBT601), colorOf(fixture.ColorBT709)}
	d, err := ResolveColor(reps, srcs, PolicyKeepSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Conflict {
		t.Fatal("conflict not detected")
	}
	for i, rc := range d.Reps {
		if rc.Decision != "keep-source" || *rc.Effective != *srcs[i] {
			t.Fatalf("rep %s: %+v", rc.Rep, rc)
		}
		if rc.Source == nil {
			t.Fatalf("rep %s: source not recorded", rc.Rep)
		}
	}
}

func TestColorConflictUnifyTarget(t *testing.T) {
	target := colorOf(fixture.ColorBT709)
	reps := []string{"800k", "1800k"}
	srcs := []*isobmff.ColorInfo{colorOf(fixture.ColorBT601), colorOf(fixture.ColorBT709)}
	d, err := ResolveColor(reps, srcs, PolicyUnifyTarget, target)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Conflict {
		t.Fatal("conflict not detected")
	}
	if d.Reps[0].Decision != "unified-to-target" || *d.Reps[0].Effective != *target {
		t.Fatalf("rep 0: %+v", d.Reps[0])
	}
	if d.Reps[1].Decision != "keep-source" {
		t.Fatalf("rep 1: %+v", d.Reps[1])
	}
}

func TestColorNoConflict(t *testing.T) {
	reps := []string{"a", "b"}
	srcs := []*isobmff.ColorInfo{colorOf(fixture.ColorBT709), colorOf(fixture.ColorBT709)}
	d, err := ResolveColor(reps, srcs, PolicyKeepSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Conflict {
		t.Fatal("false conflict")
	}
}

func TestColorUnifyRequiresTarget(t *testing.T) {
	if _, err := ResolveColor([]string{"a"}, []*isobmff.ColorInfo{nil}, PolicyUnifyTarget, nil); err == nil {
		t.Fatal("expected error for missing target")
	}
}
