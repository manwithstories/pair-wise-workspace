package model

import "testing"

func TestRationalToNanoseconds(t *testing.T) {
	// 3000 ticks at 1/90000 s = 1/30 s.
	if got := ToNanoseconds(3000, Rational{Num: 1, Den: 90000}); got != 33_333_333 {
		t.Fatalf("got %d", got)
	}
	// 44100 Hz grid: one second holds exactly 44100 samples, so the 1s
	// boundary is aligned even though one sample is not an integer ns count.
	if !SampleAligned(NanosPerSecond, 44100) {
		t.Fatal("1s boundary rejected as unaligned at 44.1kHz")
	}
	if !SampleAligned(NanosPerSecond/2, 48000) {
		t.Fatal("500ms boundary rejected as unaligned at 48kHz")
	}
	if SampleAligned(12345, 48000) {
		t.Fatal("unaligned point accepted")
	}
}

func TestResampleNoDrift(t *testing.T) {
	// Map 44.1k input onto 48k output: after 48000 output samples exactly
	// 44100 input samples must be consumed.
	p := NewResamplePosition(44100, 48000)
	var last uint64
	for i := 0; i < 48000; i++ {
		last = p.Advance()
	}
	if last != 44100 {
		t.Fatalf("consumed %d input samples, want 44100", last)
	}
}

func TestFaultAndExitCode(t *testing.T) {
	f := NewFault(FaultBadCRC, 2, 7, 100, 200, true, "x")
	if ExitCode(f) != ExitOK {
		t.Fatal("recoverable fault must map to exit 0")
	}
	g := NewFault(FaultPTSRegression, 2, 8, 0, 1, false, "y")
	if ExitCode(g) != ExitUnrecoverable {
		t.Fatal("unrecoverable fault must map to exit 3")
	}
	if ExitCode(nil) != ExitOK {
		t.Fatal("nil error must map to exit 0")
	}
}
