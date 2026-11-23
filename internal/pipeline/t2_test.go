package pipeline

import "testing"

func TestRefEq(t *testing.T) {
	dirA := t.TempDir()
	cfg := Config{Seed: 42, Segments: 4, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: dirA, Faults: Faults{Reorder: true, Truncate: true, Drift: true},
		Inject: InjectDiskFull}
	if _, err := Run(cfg); err == nil {
		t.Fatalf("disk-full run succeeded")
	}
}
