package clock

import (
	"errors"
	"testing"
	"time"
)

func TestUnwrapperHandlesWrap(t *testing.T) {
	var u Unwrapper
	start := Wrap - 2
	for i := int64(0); i < 6; i++ {
		got := u.Unwrap(uint64((start + i) % Wrap))
		if got != start+i {
			t.Fatalf("step %d = %d, want %d", i, got, start+i)
		}
	}
}

func TestUnwrapperRejectsHalfWrapJump(t *testing.T) {
	var u Unwrapper
	u.Unwrap(uint64(Wrap / 2))
	got := u.Unwrap(uint64(Wrap/2 + 1))
	if got != Wrap/2+1 {
		t.Fatalf("got %d", got)
	}
	if got := u.Unwrap(0); got != Wrap {
		t.Fatalf("wrap by full range = %d", got)
	}
}

func TestTimelinePCRWrapAndExtension(t *testing.T) {
	var tl Timeline
	base := uint64(Wrap - 3)
	for i := 0; i < 5; i++ {
		v, err := tl.PCR((base+uint64(i))%uint64(Wrap), 0)
		if err != nil {
			t.Fatal(err)
		}
		if v != (int64(base)+int64(i))*300 {
			t.Fatalf("pcr %d = %d", i, v)
		}
	}
	if _, err := tl.PCR((base+2)%uint64(Wrap), 0); !IsCode(err, CodePCRNonMonotonic) {
		t.Fatalf("want PCR regression, got %v", err)
	}
}

func TestPTS27MHz(t *testing.T) {
	var tl Timeline
	got := tl.PTS(90000)
	if got != TicksPerSecond {
		t.Fatalf("PTS = %d", got)
	}
}

func TestCheckSync(t *testing.T) {
	drift, err := CheckSync(1000, 1000+int64(39*TicksPerMilli))
	if err != nil || drift > SyncTolerance {
		t.Fatalf("drift=%v err=%v", drift, err)
	}
	if _, err := CheckSync(0, int64(41*TicksPerMilli)); !IsCode(err, CodeSyncDrift) {
		t.Fatalf("want sync drift, got %v", err)
	}
	if err != nil && IsRecoverable(err) {
		t.Fatal("sync drift must be fatal")
	}
}

func TestForwardAndCodes(t *testing.T) {
	if err := Forward(10, 9, "pts"); !IsCode(err, CodeTimestampRegression) {
		t.Fatalf("got %v", err)
	}
	if err := Forward(9, 10, "pts"); err != nil {
		t.Fatal(err)
	}
	f := FatalError(CodeUnrecoverable, "x", "bad")
	r := RecoverableError(CodeGap, "x", "gap")
	if IsRecoverable(f) || !IsRecoverable(r) {
		t.Fatal("recoverability flags wrong")
	}
	var e *Error
	if !errors.As(f, &e) || e.Error() == "" {
		t.Fatal("error string")
	}
	_ = time.Second
}
