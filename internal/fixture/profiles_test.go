package fixture

import (
	"bytes"
	"testing"

	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
)

// TestAllFaultProfilesReproducible verifies the four required fault
// families are each generated and recognized by the inspection layer.
func TestAllFaultProfilesReproducible(t *testing.T) {
	t.Run("truncated_box", func(t *testing.T) {
		pkg := mustGenerate(t, ProfileTruncated)
		rep, err := inspect.WalkBoxes(bytes.NewReader(pkg.MP4), int64(len(pkg.MP4)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		assertFaultKind(t, rep.Faults, model.KindTruncatedBox)
	})

	t.Run("corrupt_sample", func(t *testing.T) {
		pkg := mustGenerate(t, ProfileCorrupt)
		rep := inspect.AnalyzeTrack(pkg.Samples["video"], inspect.DefaultKeyframePolicy())
		if len(rep.CorruptSpans) == 0 {
			t.Fatal("corrupt profile produced no corrupt span")
		}
	})

	t.Run("missing_keyframe", func(t *testing.T) {
		pkg := mustGenerate(t, ProfileNoKeyframe)
		rep := inspect.AnalyzeTrack(pkg.Samples["video"], inspect.DefaultKeyframePolicy())
		if !rep.MissingSync {
			t.Fatal("no_keyframe profile still has a sync sample")
		}
	})

	t.Run("clock_drift", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Profile = ProfileDrift
		cfg.DriftMs = 200
		pkg, err := Generate(cfg)
		if err != nil {
			t.Fatal(err)
		}
		v := pkg.Samples["video"][0].StartMs
		a := pkg.Samples["audio"][0].StartMs
		if d := abs(a - v); d < 200 {
			t.Fatalf("drift not applied: abs(%d-%d)=%d", a, v, d)
		}
	})
}

func mustGenerate(t *testing.T, p Profile) *Package {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Profile = p
	pkg, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func assertFaultKind(t *testing.T, faults []model.Fault, want model.Kind) {
	t.Helper()
	for _, f := range faults {
		if f.Kind == want {
			return
		}
	}
	t.Fatalf("expected fault kind %s, got %v", want, faults)
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
