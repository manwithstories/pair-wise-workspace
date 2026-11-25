package media

import (
	"errors"
	"testing"
	"time"
)

func TestClockDeclarationAndTicks(t *testing.T) {
	c := DefaultClock()
	if err := c.Validate(StageClock); err != nil {
		t.Fatal(err)
	}
	if got := DurationToTicks(time.Second); got != 90000 {
		t.Fatalf("DurationToTicks=%d", got)
	}
	if got := PCMTicks(48000, 48000); got != 90000 {
		t.Fatalf("PCMTicks=%d", got)
	}
	var missing *ClockDeclaration
	if err := missing.Validate(StageClock); err == nil {
		t.Fatal("expected missing clock error")
	}
}

func TestTimelineTrackerClassifiesFaults(t *testing.T) {
	tests := []struct {
		name string
		pts  []int64
		want Kind
	}{
		{"duplicate", []int64{0, 3000, 3000}, KindDuplicate},
		{"out_of_order", []int64{0, 3000, 2999}, KindOutOfOrder},
		{"gap", []int64{0, 3000, 9000}, KindGap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := TimelineTracker{Stage: StageIndex, ExpectedStep: 3000}
			var err error
			for i, pts := range tt.pts {
				err = tr.Observe(pts, int64(i))
				if err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("expected timeline error")
			}
			var me *Error
			if !errors.As(err, &me) || me.Kind != tt.want {
				t.Fatalf("err=%v want kind=%s", err, tt.want)
			}
		})
	}
}

func TestRTPWrapIsClassifiedNotRewritten(t *testing.T) {
	tr := TimelineTracker{Stage: StageIndex}
	if err := tr.ObserveRTPTimestamp(0xffffd968, 12); err != nil { // MaxUint32-9000
		t.Fatal(err)
	}
	err := tr.ObserveRTPTimestamp(80999, 88)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindWrap {
		t.Fatalf("wrap error=%v", err)
	}
	if me.Offset != 88 || !me.HasPTS {
		t.Fatalf("error metadata=%+v", me)
	}
}

func TestExitCodeMapping(t *testing.T) {
	retry := E(StageWAL, KindInterrupted, 3, errors.New("crash")).WithRetryable()
	if code := ExitCode(retry); code != 75 {
		t.Fatalf("retry code=%d", code)
	}
	resource := E(StageQueue, KindResource, 0, errors.New("memory"))
	if code := ExitCode(resource); code != 3 {
		t.Fatalf("resource code=%d", code)
	}
	invalid := E(StageIngest, KindTruncated, 9, nil)
	if code := ExitCode(invalid); code != 2 {
		t.Fatalf("invalid code=%d", code)
	}
	if code := ExitCode(nil); code != 0 {
		t.Fatalf("nil code=%d", code)
	}
}
