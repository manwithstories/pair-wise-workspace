package probe

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fault"
)

// TestFragmentedFileIsRecognized checks a moof-bearing file is flagged as
// fragmented with an explicit unsupported notice rather than a generic error.
func TestFragmentedFileIsRecognized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frag.mp4")
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41"))
	moov := box("moov", nil)
	moof := box("moof", nil)
	mdat := box("mdat", make([]byte, 16))
	if err := os.WriteFile(path, concat(ftyp, moov, moof, mdat), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Analyze(path, DefaultConfig())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if !f.Fragmented || f.MOOFCount != 1 {
		t.Fatalf("fragmented=%v moof=%d", f.Fragmented, f.MOOFCount)
	}
	var noticed bool
	for _, is := range f.TrackIssues() {
		if is.Code == fault.CodeUnsupported && is.Severity == fault.SeverityWarning {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("no unsupported notice: %v", f.TrackIssues())
	}
}
