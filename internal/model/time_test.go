package model

import "testing"

func TestPTS90kToTicks(t *testing.T) {
	if got := PTS90kToTicks(90000); got != Ticks(ClockHz) {
		t.Fatalf("1s of PTS = %d ticks, want %d", got, ClockHz)
	}
	if got := PTS90kToTicks(3003); got != 900900 {
		t.Fatalf("3003 PTS units = %d ticks, want 900900", got)
	}
}

func TestRawPCRToTicks(t *testing.T) {
	// base=45000 (0.5s), ext=13 -> 0.5s in ticks + 13
	if got := RawPCRToTicks(45000, 13); got != Ticks(ClockHz/2+13) {
		t.Fatalf("RawPCRToTicks = %d", got)
	}
}

func TestMillis(t *testing.T) {
	if got := Ticks(ClockHz).Millis(); got != 1000.0 {
		t.Fatalf("1s in millis = %v", got)
	}
}

func TestFaultRecoverable(t *testing.T) {
	if FaultUnanchoredEpoch.Recoverable() || FaultBoundary.Recoverable() {
		t.Fatal("unanchored epoch and boundary conflict must be unrecoverable")
	}
	if !FaultContinuityGap.Recoverable() || !FaultPSICRC.Recoverable() {
		t.Fatal("continuity gap and PSI CRC faults must be recoverable")
	}
	f := Fault{Kind: FaultSyncLoss, Offset: 188, PID: 0x100, Detail: "x"}
	if f.Error() == "" || f.Kind.String() != "sync_loss" {
		t.Fatalf("bad fault rendering: %v", f)
	}
}
