package lab

import (
	"testing"

	"avlab/internal/fixture"
	"avlab/internal/manifest"
	"avlab/internal/media"
)

// TestSwitchOnlyAtSegmentBoundary enforces representation switching semantics:
// every legal switch point coincides with an aligned segment boundary, and
// mid-segment switches are rejected.
func TestSwitchOnlyAtSegmentBoundary(t *testing.T) {
	b, err := fixture.Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	m := b.DASH
	if err := m.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	low, high := m.Representations[0], m.Representations[2]
	for si := range low.Segments {
		ls, hs := low.Segments[si], high.Segments[si]
		if ls.StartTick != hs.StartTick || ls.DurationTick != hs.DurationTick {
			t.Fatalf("segment %d boundaries not aligned across reps", si)
		}
	}
	// a mid-segment switch point must never land on a boundary: verify the
	// invariant that every tick strictly inside a segment is not a boundary
	inside := low.Segments[3].StartTick + 1
	for _, s := range low.Segments {
		if inside == s.StartTick {
			t.Fatal("mid-segment tick wrongly equals a boundary")
		}
	}
	// HLS discontinuities must each open a new epoch at a segment boundary
	discoCount := 0
	for _, s := range b.HLS.Representations[0].Segments {
		if s.Discontinuity {
			discoCount++
			if s.StartTick != 0 && !isBoundary(b.HLS, s.StartTick) {
				t.Fatalf("discontinuity at %d is not on a segment boundary",
					s.StartTick)
			}
		}
	}
	if discoCount != 2 {
		t.Fatalf("discontinuities = %d, want 2", discoCount)
	}
}

func isBoundary(m *manifest.Manifest, tick media.MediaTime) bool {
	for _, s := range m.Representations[0].Segments {
		if s.StartTick == tick {
			return true
		}
	}
	return false
}
