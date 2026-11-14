package fixture

import (
	"bytes"
	"testing"
	"time"

	"tsprobe/internal/tspacket"
)

func TestGenerateSummaryAndPacketShape(t *testing.T) {
	var buf bytes.Buffer
	summary, err := Generate(&buf, Options{
		Programs: 2, Duration: 12 * time.Second, Loss: 7, CCJumps: 3, Versions: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.VideoPackets != 1180 || summary.PMTPackets != 24 || summary.Lost != 7 ||
		summary.CCJumps != 3 || summary.PATVersions != 4 || summary.PMTVersions != 3 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(buf.Bytes()) == 0 || buf.Bytes()[0] != tspacket.SyncByte {
		t.Fatalf("stream does not start with a sync byte")
	}
	if len(buf.Bytes()) != 230314 {
		t.Fatalf("sample size=%d", len(buf.Bytes()))
	}
}
