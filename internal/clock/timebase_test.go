package clock

import (
	"testing"
	"time"
)

func TestTimebaseConversions(t *testing.T) {
	base := Default()
	if got := base.TicksFromDuration(2 * time.Second); got != 180000 {
		t.Fatalf("2s ticks = %d", got)
	}
	if got := base.Duration(45000); got != 500*time.Millisecond {
		t.Fatalf("duration = %v", got)
	}
	audio := MustNew(48000)
	if got := audio.Convert(96000, base); got != 180000 {
		t.Fatalf("48000->90000 = %d", got)
	}
	if got := base.Convert(180000, audio); got != 96000 {
		t.Fatalf("90000->48000 = %d", got)
	}
	if got := audio.Convert(1, base); got != 2 { // 1/48000 rounds to 2/90000
		t.Fatalf("rounding = %d", got)
	}
}
