package model

import "testing"

func TestUnwrap(t *testing.T) {
	cases := []struct {
		name string
		prev uint64
		raw  uint64
		want uint64
	}{
		{"plain", 1000, 1042, 1042},
		{"rollover", RTPMod - 10, 20, RTPMod + 20},
		{"after rollover", RTPMod + 20, 60, RTPMod + 60},
		{"small backwards step stays", 500, 490, 490},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unwrap(tc.prev, tc.raw); got != tc.want {
				t.Fatalf("Unwrap(%d,%d)=%d want %d", tc.prev, tc.raw, got, tc.want)
			}
		})
	}
	if !IsRollover(RTPMod-10, 20) {
		t.Fatal("rollover not detected at modulus boundary")
	}
}

func TestClockConversionRoundTrip(t *testing.T) {
	c := Clock{TrackID: "a1", Rate: 48000, AnchorSeq: 0, AnchorMasterTS: 0}
	// 48000 audio ticks = one second = 90000 master ticks.
	if got := c.ToMaster(48000); got != MasterRate {
		t.Fatalf("ToMaster=%d want %d", got, MasterRate)
	}
	if got := MasterToNano(MasterRate); got != 1_000_000_000 {
		t.Fatalf("MasterToNano=%d", got)
	}
}

func TestExitCodePrecedence(t *testing.T) {
	if got := ExitCode([]Fault{{Class: FaultChecksum}}); got != 2 {
		t.Fatalf("checksum exit=%d want 2", got)
	}
	if got := ExitCode([]Fault{
		{Class: FaultChecksum},
		{Class: FaultGraphCycle},
	}); got != 3 {
		t.Fatalf("graph exit=%d want 3", got)
	}
	if got := ExitCode([]Fault{{Class: FaultCheckpoint}}); got != 4 {
		t.Fatalf("checkpoint exit=%d want 4", got)
	}
}
