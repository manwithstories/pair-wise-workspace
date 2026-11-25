package clock_test

import (
	"math"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

func TestRationalRejectsOverflow(t *testing.T) {
	max := clock.Must(math.MaxInt64, 1)
	_, err := clock.Add(max, clock.One)
	if f, ok := fault.As(err); !ok || f.Code != fault.TimestampOverflow {
		t.Fatalf("Add overflow err=%v want E_TIMESTAMP_OVERFLOW", err)
	}
	_, err = clock.Mul(clock.Must(math.MaxInt64/2, 1), clock.Must(8, 1))
	if f, ok := fault.As(err); !ok || f.Code != fault.TimestampOverflow {
		t.Fatalf("Mul overflow err=%v want E_TIMESTAMP_OVERFLOW", err)
	}
}

func TestTimelineRejectsBackwardAndMismatch(t *testing.T) {
	tl := clock.NewTimeline("video")
	dur := clock.Must(1, 30)
	zero := clock.Zero
	if err := tl.Observe("seg0", clock.Stamp{Period: 0, PTS: zero, DTS: zero, Duration: dur}, "init0"); err != nil {
		t.Fatal(err)
	}
	back := clock.Must(-1, 30)
	err := tl.Observe("seg1", clock.Stamp{Period: 0, PTS: back, DTS: back, Duration: dur}, "init0")
	if f, ok := fault.As(err); !ok || f.Code != fault.TimestampBackward {
		t.Fatalf("backward err=%v", err)
	}

	wrongStart := clock.Must(2, 30)
	err = tl.Observe("seg2", clock.Stamp{Period: 0, PTS: wrongStart, DTS: wrongStart, Duration: dur}, "init0")
	if f, ok := fault.As(err); !ok || f.Code != fault.SegmentMismatch {
		t.Fatalf("mismatch err=%v", err)
	}
}

func TestDiscontinuityStartsNewPeriod(t *testing.T) {
	tl := clock.NewTimeline("audio")
	dur := clock.Must(1, 3)
	for period := 0; period < 2; period++ {
		err := tl.Observe("seg", clock.Stamp{
			Period: period, PTS: clock.Zero, DTS: clock.Zero, Duration: dur,
		}, "init")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(tl.Periods) != 2 {
		t.Fatalf("periods=%d want 2", len(tl.Periods))
	}
	total, err := tl.Duration()
	if err != nil {
		t.Fatal(err)
	}
	if want := clock.Must(2, 3); clock.Cmp(total, want) != 0 {
		t.Fatalf("duration=%s want %s", total, want)
	}
}
