package audio

import (
	"testing"
	"time"
)

func TestSilenceWindowsOnUnifiedClock(t *testing.T) {
	// 16 kHz mono: 20 ms = 320 samples = 640 bytes per window.
	pcm := append(TonePCM(320, 12000), TonePCM(4*320, 0)...)
	pcm = append(pcm, TonePCM(320, 12000)...)
	got, err := Analyze(2*time.Second, pcm, Config{MinSilenceMS: 80})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("silence intervals=%+v, want one", got)
	}
	wantStart := 2*time.Second + 20*time.Millisecond
	if got[0].Start != wantStart || got[0].End != wantStart+80*time.Millisecond {
		t.Fatalf("interval=%s..%s, want %s..%s", got[0].Start, got[0].End, wantStart, wantStart+80*time.Millisecond)
	}
}

func TestShortSilenceRejectedByMillisecondThreshold(t *testing.T) {
	pcm := append(TonePCM(320, 10000), TonePCM(320, 0)...)
	pcm = append(pcm, TonePCM(320, 10000)...)
	got, err := Analyze(0, pcm, Config{MinSilenceMS: 80})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("20ms silence passed 80ms threshold: %+v", got)
	}
}

func TestUnalignedPCMRejected(t *testing.T) {
	if _, err := Analyze(0, []byte{1, 0, 2}, Config{}); err == nil {
		t.Fatal("expected alignment error")
	}
}
