package pipeline

import "testing"

func TestSmoke(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(Config{Seed: 42, Segments: 12, SegmentDurNS: 4_000_000_000,
		PacketRate: 50, WorkDir: dir, Faults: Faults{Reorder: true, Truncate: true, Drift: true}})
	t.Logf("res=%+v err=%v", res, err)
}
