package report

import (
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := Percentile(v, 50); got != 5.5 {
		t.Fatalf("p50 %v", got)
	}
	if got := Percentile(v, 100); got != 10 {
		t.Fatalf("p100 %v", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty %v", got)
	}
}

func TestAVSyncErrors(t *testing.T) {
	// 90 kHz common timebase: 90 ticks = 1 ms
	video := []int64{0, 3000, 6000}
	audio := []int64{90, 3090, 6180} // 1ms, 1ms, 2ms offset
	errs := AVSyncErrors(video, audio)
	if len(errs) != 3 || errs[0] != 1 || errs[2] != 2 {
		t.Fatalf("errs %v", errs)
	}
	if AVSyncErrors(video, nil) != nil {
		t.Fatal("expected nil for missing audio")
	}
}

func TestWriteJSON(t *testing.T) {
	var b strings.Builder
	s := &Summary{PacketRecoveryRate: 0.97, CheckpointResumeVerified: true}
	if err := WriteJSON(&b, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `"checkpoint_resume_verified": true`) {
		t.Fatalf("json %s", b.String())
	}
}
