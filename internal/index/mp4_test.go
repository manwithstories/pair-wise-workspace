package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abema/go-mp4"

	"github.com/pairwise/mediapack/internal/cut"
)

func TestMP4FreeBoxSidecarRoundTrip(t *testing.T) {
	segs := []*cut.Segment{
		{
			ID: "clip-0001", Index: 0, Start: 0, End: 80 * time.Millisecond,
			ClipSHA256: "a", ContentFP: "b", Committable: true,
			Keyframes: []cut.KeyframeRef{{Sequence: 7, PTS: 0, PTSTicks: 0, PSIVersion: 2}},
		},
	}
	sc := FromSegments(73, segs)
	b, err := MarshalSidecar(sc)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 16 || string(b[4:8]) != "free" {
		t.Fatalf("not an MP4 free box: %x", b[:min(16, len(b))])
	}

	path := filepath.Join(t.TempDir(), "keyframes.mp4idx")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSidecarFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seed != 73 || len(got.Entries) != 1 || got.Entries[0].Sequence != 7 {
		t.Fatalf("bad sidecar: %+v", got)
	}
	if hit := KeyframeHitRate(got, map[string]string{"clip-0001": "a"}); hit != 100 {
		t.Fatalf("hit rate=%v, want 100", hit)
	}
}

func TestReadSidecarRejectsNonFreeBox(t *testing.T) {
	// A minimal ftyp box is a valid MP4 box but not our free sidecar.
	b := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'M', 'P', '4', '2', 0, 0, 0, 0}
	path := filepath.Join(t.TempDir(), "x.mp4")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	_, bi, err := ReadSidecar(f)
	if err == nil {
		t.Fatal("expected non-free error")
	}
	if bi == nil || bi.Type != mp4.BoxTypeFtyp() {
		t.Fatalf("box info=%+v", bi)
	}
}
