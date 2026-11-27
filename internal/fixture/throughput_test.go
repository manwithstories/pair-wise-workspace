package fixture_test

import (
	"testing"
	"time"

	"relayaudit/internal/fixture"
	"relayaudit/internal/wire"
)

func TestScanThroughputIsReasonable(t *testing.T) {
	seg, err := fixture.Generate(fixture.Spec{Seed: 73, Size: 8388608})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	start := time.Now()
	if _, err := wire.Scan(seg.Bytes); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("scanning %d bytes took %v", len(seg.Bytes), d)
	} else {
		t.Logf("scanned %d bytes in %v", len(seg.Bytes), d)
	}
}
