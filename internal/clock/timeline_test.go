package clock

import (
	"testing"
	"time"
)

func TestWrap16(t *testing.T) {
	cases := []struct {
		a, b uint16
		want int
	}{
		{10, 12, 2},
		{12, 10, -2},
		{65534, 1, 3},  // forward wrap
		{1, 65534, -3}, // backward wrap
		{0, 0, 0},
		{32767, 0, -32767},
	}
	for _, c := range cases {
		if got := Wrap16(c.a, c.b); got != c.want {
			t.Errorf("Wrap16(%d,%d)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestWrap32(t *testing.T) {
	if got := Wrap32(0xFFFFFFF0, 0x00000010); got != 32 {
		t.Errorf("forward wrap: got %d want 32", got)
	}
	if got := Wrap32(0x00000010, 0xFFFFFFF0); got != -32 {
		t.Errorf("backward wrap: got %d want -32", got)
	}
}

func TestTickRoundTripSamples(t *testing.T) {
	for _, rate := range []uint32{8000, 48000} {
		for _, samples := range []int64{0, 1, 240, 480, 1920, 48000, 480000} {
			tk := FromSamples(samples, rate)
			got := tk.ToSamples(rate)
			if got != samples {
				t.Errorf("rate=%d samples=%d round trip=%d", rate, samples, got)
			}
		}
	}
}

func TestTimelineCalibration(t *testing.T) {
	tl := NewTimeline(48000)
	if tl.Valid() {
		t.Fatal("timeline should be invalid before first report")
	}
	// 48 kHz RTP ts 48000 == 1 s; reference tick 90000.
	if err := tl.Calibrate(48000, FromTime(time.Second)); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	tk, err := tl.ToTick(96000)
	if err != nil {
		t.Fatalf("totick: %v", err)
	}
	if tk != FromTime(2*time.Second) {
		t.Errorf("got tick %d want %d", tk, FromTime(2*time.Second))
	}
	// A report jumping 2 seconds away from the implied position must be rejected.
	err = tl.Calibrate(144000, FromTime(4*time.Second))
	if err == nil || !IsCode(err, ErrReportJump) {
		t.Fatalf("expected report jump error, got %v", err)
	}
	// Last valid mapping retained.
	tk2, _ := tl.ToTick(96000)
	if tk2 != tk {
		t.Errorf("mapping changed after rejected report: %d != %d", tk2, tk)
	}
}

func TestNoReportProvisional(t *testing.T) {
	tl := NewTimeline(90000)
	_, err := tl.ToTick(90000)
	if !IsCode(err, ErrNoReport) {
		t.Fatalf("expected ErrNoReport, got %v", err)
	}
}

func TestCheckDuration(t *testing.T) {
	if err := CheckDuration(time.Minute); err != nil {
		t.Errorf("1 min should pass: %v", err)
	}
	if err := CheckDuration(11 * time.Minute); !IsCode(err, ErrDurationTooLong) {
		t.Fatalf("expected duration error, got %v", err)
	}
}
