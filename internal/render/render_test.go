package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediasync/internal/metadata"
	"mediasync/internal/model"
	"mediasync/internal/timeline"
)

func buildTimeline(t *testing.T, withGap bool) *timeline.Timeline {
	t.Helper()
	master := model.Clock90k
	var frames []model.Frame
	frames = append(frames,
		model.Frame{Track: "v", Rep: "high", Seq: 0, Timestamp: 0, Clock: master, Payload: []byte{1}},
		model.Frame{Track: "v", Rep: "high", Seq: 1, Timestamp: 3000, Clock: master, Payload: []byte{2}})
	if withGap {
		frames = append(frames, model.Frame{Track: "v", Rep: "low", Seq: 2, Clock: master, Incomplete: true})
	}
	frames = append(frames,
		model.Frame{Track: "v", Rep: "low", Seq: 3, Timestamp: 9000, Clock: master,
			Recovered: true, Payload: []byte{4}})
	in := timeline.Input{Track: metadata.ResolvedTrack{Track: metadata.TrackMeta{
		TrackID: "cam1", Kind: model.KindVideo, Timescale: 90000,
		FrameRate: model.Rate30, Source: "mxf"}}, Frames: frames}
	return timeline.Merge(master, []timeline.Input{in}, model.Rational{Num: 20, Den: 1000})
}

func TestExportDeterministicHashesAndManifest(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	tl := buildTimeline(t, false)
	frags := timeline.Fragmentize(tl)
	mismatches, _ := timeline.VerifyBoundaries(tl, frags)

	exp1, err := NewExporter(filepath.Join(dir1, "out"))
	if err != nil {
		t.Fatal(err)
	}
	m1, err := exp1.Export(tl, mismatches)
	if err != nil {
		t.Fatal(err)
	}
	exp2, err := NewExporter(filepath.Join(dir2, "out"))
	if err != nil {
		t.Fatal(err)
	}
	m2, err := exp2.Export(tl, mismatches)
	if err != nil {
		t.Fatal(err)
	}
	if m1.FrameCount == 0 || m1.HashCount != m1.FrameCount {
		t.Fatalf("every complete frame must carry a hash: %+v", m1)
	}
	for i := range m1.Frames {
		if m1.Frames[i].SHA256 != m2.Frames[i].SHA256 {
			t.Fatalf("frame %d hash differs across runs: %s vs %s",
				i, m1.Frames[i].SHA256, m2.Frames[i].SHA256)
		}
		if m1.Frames[i].FrameFile != m2.Frames[i].FrameFile {
			t.Fatal("frame file naming must be stable")
		}
	}
	// Manifest on disk parses and matches the returned object's checksum.
	raw, err := os.ReadFile(filepath.Join(dir1, "out", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Manifest
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Checksum == "" || onDisk.Checksum != m1.Checksum {
		t.Fatal("manifest checksum mismatch")
	}
	entries, err := os.ReadDir(filepath.Join(dir1, "out", "frames"))
	if err != nil || len(entries) != m1.FrameCount {
		t.Fatalf("frame files missing: entries=%d count=%d err=%v", len(entries), m1.FrameCount, err)
	}
}

// TestExportFaultPropagation covers the required second propagation path:
// transport gaps and boundary mismatches reach the export manifest.
func TestExportFaultPropagation(t *testing.T) {
	dir := t.TempDir()
	tl := buildTimeline(t, true)
	// Use a deliberately corrupted boundary fixture so mismatches propagate into
	// the manifest alongside the transport-gap incomplete trail.
	corrupt := []timeline.Fragment{
		{TrackID: "cam1", Rep: "high", FirstTick: tl.Slots[0].Tick, EndTick: tl.Slots[2].Tick, Frames: 2},
		{TrackID: "cam1", Rep: "low", FirstTick: tl.Slots[1].Tick, EndTick: tl.Slots[3].Tick, Frames: 2},
	}
	mismatches, bag := timeline.VerifyBoundaries(tl, corrupt)
	timeline.MergeDiag(tl, bag)
	if len(mismatches) == 0 {
		t.Fatal("corrupted fixture must create a boundary mismatch")
	}
	exp, err := NewExporter(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := exp.Export(tl, mismatches)
	if err != nil {
		t.Fatal(err)
	}
	var sawIncomplete, sawRecovered bool
	for _, f := range m.Frames {
		if f.Incomplete && f.Conclusion == "incomplete_gap_placeholder" {
			sawIncomplete = true
			if f.SHA256 == "" {
				t.Fatal("gap placeholder still receives a review hash")
			}
		}
		if f.Recovered && f.Conclusion == "recovered_within_window" {
			sawRecovered = true
		}
	}
	if !sawIncomplete || !sawRecovered {
		t.Fatalf("manifest must record both conclusions: incomplete=%v recovered=%v", sawIncomplete, sawRecovered)
	}
	if len(m.Incomplete) != 1 || m.Incomplete[0] != "cam1" {
		t.Fatalf("incomplete track must propagate to manifest: %v", m.Incomplete)
	}
	if len(m.Mismatches) != len(mismatches) || len(m.Diagnostics) == 0 {
		t.Fatalf("boundary mismatches/diagnostics must propagate: %+v", m)
	}
}

func TestExportRejectsPathTraversal(t *testing.T) {
	base := t.TempDir()
	for _, bad := range []string{
		base + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "escape",
		base + string(os.PathSeparator) + "legit" + string(os.PathSeparator) + ".." +
			string(os.PathSeparator) + ".." + string(os.PathSeparator) + "escape2",
	} {
		if _, err := NewExporter(bad); err == nil {
			t.Fatalf("traversal path must be rejected: %s", bad)
		}
	}
	// A normal nested directory succeeds and stays inside the root.
	exp, err := NewExporter(filepath.Join(base, "nested", "out"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(exp.Root(), base) {
		t.Fatalf("resolved root escaped base: %s", exp.Root())
	}
	if _, err := NewExporter("   "); err == nil {
		t.Fatal("blank output path must be rejected")
	}
}

func TestSanitizeTrackName(t *testing.T) {
	cases := map[string]string{
		"cam/1": "cam_1",
		"a..b":  "a_b",
		"x\\y":  "x_y",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Fatalf("sanitize(%q)=%q want %q", in, got, want)
		}
	}
}
