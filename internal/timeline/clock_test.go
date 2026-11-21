package timeline

import "testing"

func TestClockConversions(t *testing.T) {
	c := NewClock(1_000_000) // 1 ms per timecode unit
	if got := c.ToNanoseconds(250); got != 250_000_000 {
		t.Fatalf("positive tc: %d", got)
	}
	if got := c.ToNanoseconds(-3); got != -3_000_000 {
		t.Fatalf("negative tc: %d", got)
	}
	c2 := NewClock(0) // zero must fall back to default
	if c2.ScaleNS() != DefaultTimestampScale {
		t.Fatalf("default scale fallback: %d", c2.ScaleNS())
	}
	c3 := NewClock(2_500_000)
	if got := c3.ToNanoseconds(4); got != 10_000_000 {
		t.Fatalf("custom scale: %d", got)
	}
}

func TestRoundMillisHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		ns   int64
		want int64
	}{
		{1_499_999, 1}, {1_500_000, 2}, {1_500_001, 2},
		{-1_499_999, -1}, {-1_500_000, -2}, {0, 0},
	}
	for _, c := range cases {
		if got := RoundMillis(c.ns); got != c.want {
			t.Fatalf("RoundMillis(%d)=%d want %d", c.ns, got, c.want)
		}
	}
}

func TestAlignmentErrorMS(t *testing.T) {
	if got := AlignmentErrorMS(2_300_000, 2_000_000); got != 0.3 {
		t.Fatalf("alignment: %v", got)
	}
	if got := AlignmentErrorMS(1_000_000, 1_000_999); got != 0.001 {
		t.Fatalf("sub-ms alignment: %v", got)
	}
	if got := AlignmentErrorMS(0, -5_000_000); got != 5 {
		t.Fatalf("absolute difference: %v", got)
	}
}

func TestManifestDeterminism(t *testing.T) {
	build := func() *Manifest {
		b := NewBuilder(1_000_000)
		b.Observe(2, 2, "audio", 100)
		b.Observe(1, 1, "video", 0)
		b.Observe(2, 2, "audio", 200)
		return b.Build()
	}
	m1, m2 := build(), build()
	if len(m1.Tracks) != 2 || m1.Tracks[0].ArchiveTrackID != 1 {
		t.Fatalf("tracks not sorted by archive id: %+v", m1.Tracks)
	}
	if m1.Tracks[1].StartNS != 100_000_000 || m1.Tracks[1].EndNS != 200_000_000 || m1.Tracks[1].Blocks != 2 {
		t.Fatalf("audio range wrong: %+v", m1.Tracks[1])
	}
	if m1.TimestampScale != m2.TimestampScale ||
		m1.Tracks[0].StartNS != m2.Tracks[0].StartNS ||
		m1.Tracks[1].EndNS != m2.Tracks[1].EndNS {
		t.Fatal("repeated builds diverged")
	}
}
