package synth

import (
	"os"
	"testing"

	"archivectl/internal/container"
	"archivectl/internal/timeline"
	"archivectl/internal/transport"
)

func TestSeed41FixturesContainAllMarkerKinds(t *testing.T) {
	files, err := Generate(t.TempDir(), 41)
	if err != nil {
		t.Fatal(err)
	}
	a, err := container.ParseFile(files.Normal)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Tracks) != 4 {
		t.Fatalf("tracks=%d want 4", len(a.Tracks))
	}
	kinds := map[string]bool{}
	for _, e := range container.CollectEvents(a) {
		kinds[e.Kind] = true
	}
	for _, kind := range []string{
		container.EventSubtitle, container.EventChapter,
		container.EventID3, container.EventEMSG, container.EventSEI,
	} {
		if !kinds[kind] {
			t.Fatalf("missing marker %s", kind)
		}
	}
}

func TestGeneratedWrapAndBurstFaults(t *testing.T) {
	files, err := Generate(t.TempDir(), 41)
	if err != nil {
		t.Fatal(err)
	}
	wrapFrames := readFrames(t, files.WrapRTP)
	wrapSegs, win, err := transport.AssembleCapture(wrapFrames, 64)
	if err != nil {
		t.Fatal(err)
	}
	if win.HasGaps() || win.Counters.DupDrop < 2 || win.Counters.Reordered < 2 {
		t.Fatalf("wrap stats=%+v gaps=%+v", win.Counters, win.Gaps)
	}
	if len(wrapSegs) != 5 { // init + four media fragments
		t.Fatalf("wrap segments=%d want 5", len(wrapSegs))
	}

	burstFrames := readFrames(t, files.BurstRTP)
	burstSegs, burstWin, err := transport.AssembleCapture(burstFrames, 64)
	if err != nil {
		t.Fatal(err)
	}
	if burstWin.Counters.GapPackets != 3 || len(burstWin.Gaps) != 1 {
		t.Fatalf("burst gaps=%+v counters=%+v", burstWin.Gaps, burstWin.Counters)
	}
	if len(burstSegs) != 4 { // init + three surviving media fragments
		t.Fatalf("burst segments=%d want 4", len(burstSegs))
	}
}

func TestGeneratedTruncatedFixtureIsCorrupt(t *testing.T) {
	files, err := Generate(t.TempDir(), 41)
	if err != nil {
		t.Fatal(err)
	}
	_, err = container.ParseFile(files.TruncatedMP4)
	if !timeline.IsCode(err, timeline.CodeCorruptBox) {
		t.Fatalf("expected corrupt_box, got %v", err)
	}
}

func readFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	frames, err := transport.ReadCapture(f)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}
