package impair

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestParseSpec(t *testing.T) {
	cfg, err := ParseSpec("delay=20ms,reorder=5,loss=0.01,duplicate=2,seed=7")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Delay != 20*time.Millisecond || cfg.ReorderPercent != 5 || cfg.Loss != 0.01 || cfg.Seed != 7 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestInjectionReproducible(t *testing.T) {
	cfg := Config{Delay: 20 * time.Millisecond, ReorderPercent: 5, Loss: 0.01, DuplicatePct: 2, Seed: 7}
	run := func() (int, uint64, uint64) {
		injector, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		base := time.Unix(1, 0)
		count := 0
		for n := 0; n < 1000; n++ {
			packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(n), Timestamp: uint32(n * 960)}}
			count += len(injector.Inject(packet, base.Add(time.Duration(n)*20*time.Millisecond)))
		}
		dropped, duplicated := injector.Snapshot()
		return count, dropped, duplicated
	}
	c1, d1, u1 := run()
	c2, d2, u2 := run()
	if c1 != c2 || d1 != d2 || u1 != u2 {
		t.Fatalf("runs differ: (%d,%d,%d) vs (%d,%d,%d)", c1, d1, u1, c2, d2, u2)
	}
}
