package ts

import (
	"testing"

	"mediqc/internal/media"
)

func makeSyncStream(packetSize, packets int) []byte {
	b := make([]byte, packetSize*packets)
	for i := 0; i < packets; i++ {
		b[i*packetSize] = SyncByte
	}
	return b
}

func TestFindFirstDesync188(t *testing.T) {
	b := makeSyncStream(Size188, 5)
	if off := FindFirstDesync(b, Size188); off != -1 {
		t.Fatalf("clean stream desync = %d, want -1", off)
	}
	b[2*Size188] = 0x00
	if off := FindFirstDesync(b, Size188); off != 2*Size188 {
		t.Fatalf("first desync = %d, want %d", off, 2*Size188)
	}
}

func TestDetectSyncResyncsAfterBadByte(t *testing.T) {
	b := makeSyncStream(Size188, 8)
	b[2*Size188] = 0x99
	report, err := DetectSync(b, media.DefaultLimits())
	if err == nil || err.Fatal {
		t.Fatalf("expected recoverable sync error, got %v", err)
	}
	if report.FirstDesync != 2*Size188 || report.ResyncOffset != 3*Size188 || report.PacketSize != Size188 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestDetectSync204(t *testing.T) {
	b := makeSyncStream(Size204, 5)
	report, err := DetectSync(b, media.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if report.PacketSize != Size204 || report.TrailerLength != 16 || report.FirstDesync != -1 {
		t.Fatalf("bad 204 report: %+v", report)
	}
}

func TestDetectSyncRejectsEmpty(t *testing.T) {
	if _, err := DetectSync(nil, media.DefaultLimits()); err == nil || err.Code != media.CodeUnexpectedEOF {
		t.Fatalf("expected unexpected EOF, got %v", err)
	}
}
