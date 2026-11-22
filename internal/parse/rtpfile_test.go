package parse

import (
	"os"
	"path/filepath"
	"testing"

	"avscan/internal/fixture"
	"avscan/internal/model"
)

func TestReadRTP1CaptureEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.rtp1")
	if err := fixture.WriteRTPCapture(path, 90); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadRTP1File(path, "voice", 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 90 {
		t.Fatalf("records = %d, want 90", len(recs))
	}
	res, err := ParseRTPStream(recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.GapCount != 3 {
		t.Errorf("gap count = %d, want injected 3", res.GapCount)
	}
	var hasGap bool
	for _, f := range res.Faults {
		if f.Kind == model.FaultRTPSeqGap {
			hasGap = true
		}
	}
	if !hasGap {
		t.Error("injected sequence gap not detected")
	}
}

func TestReadRTP1BadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.rtp1")
	if err := writeBytes(path, []byte("XXXX")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRTP1File(path, "v", 8000); err == nil {
		t.Error("bad magic must error")
	}
}

func TestReadRTP1TruncatedFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc.rtp1")
	// Valid magic, then a length header claiming 100 bytes but providing none.
	if err := writeBytes(path, append([]byte("RTP1"), 0, 0, 0, 100)); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadRTP1File(path, "v", 8000)
	if err == nil {
		t.Error("truncated frame must error")
	}
	if recs != nil && len(recs) != 0 {
		t.Errorf("no complete records expected, got %d", len(recs))
	}
}

func writeBytes(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
