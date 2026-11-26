package render

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
)

func sampleItems() []timeline.Item {
	items := make([]timeline.Item, 0, 90)
	for seq := int64(0); seq < 90; seq++ {
		items = append(items, timeline.Item{
			StreamID: 0, Seq: seq, Kind: manifest.KindMedia,
			MasterPTS: seq * clock.MasterHz, // one item per second
			Keyframe:  seq%10 == 0,
		})
	}
	return items
}

func sampleStream() manifest.Stream {
	return manifest.Stream{
		ID: 0, Kind: manifest.KindVideo, Codec: "h264", Container: "mp4",
		File: "v.mp4", Width: 24, Height: 18, TimebaseNum: 90000, KeyframeEvery: 10,
	}
}

func TestDrawFrameDeterministic(t *testing.T) {
	in := DrawInput{Seed: 41, StreamID: 0, Width: 24, Height: 18,
		AtMaster: 5 * clock.MasterHz, KeyframeMaster: 0, KeyframeSeq: 0}
	a := pngBytes(DrawFrame(in))
	b := pngBytes(DrawFrame(in))
	if len(a) == 0 || string(a) != string(b) {
		t.Fatal("same input must yield identical PNG bytes")
	}
	// A different master tick must change the image.
	in.AtMaster = 6 * clock.MasterHz
	if string(pngBytes(DrawFrame(in))) == string(a) {
		t.Fatal("different tick must change frame bytes")
	}
	// A different keyframe landing must also change the image.
	in.AtMaster = 5 * clock.MasterHz
	in.KeyframeSeq = 10
	if string(pngBytes(DrawFrame(in))) == string(a) {
		t.Fatal("different keyframe landing must change frame bytes")
	}
}

func TestExportWritesPNGAndStableHash(t *testing.T) {
	dir := t.TempDir()
	exp := NewExporter(sampleStream(), sampleItems(), nil, dir, 41)
	reqs := []FrameRequest{
		{AtMaster: 5 * clock.MasterHz, Label: "a"},
		{AtMaster: 25 * clock.MasterHz, Label: "b"},
		{AtMaster: 55 * clock.MasterHz, Label: "c"},
	}
	res, errs := exp.Export(reqs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 frames, got %d", len(res))
	}
	seen := map[string]bool{}
	for _, fr := range res {
		if fr.SHA256 == "" || len(fr.SHA256) != 64 {
			t.Fatalf("bad sha256: %q", fr.SHA256)
		}
		if seen[fr.SHA256] {
			t.Fatal("frames at different ticks must have distinct hashes")
		}
		seen[fr.SHA256] = true
		f, err := os.Open(fr.PNGPath)
		if err != nil {
			t.Fatalf("png missing: %v", err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil || cfg.Width != 24 || cfg.Height != 18 {
			t.Fatalf("decoded png dimensions wrong: %dx%d err=%v", cfg.Width, cfg.Height, err)
		}
		// Output must stay under the requested base directory.
		if rel, _ := filepath.Rel(dir, fr.PNGPath); len(rel) >= 2 && rel[:2] == ".." {
			t.Fatalf("frame escaped base dir: %s", fr.PNGPath)
		}
	}

	// Re-export with the same exporter inputs: hashes must be identical.
	exp2 := NewExporter(sampleStream(), sampleItems(), nil, t.TempDir(), 41)
	res2, _ := exp2.Export(reqs)
	for i := range res {
		if res[i].SHA256 != res2[i].SHA256 {
			t.Fatalf("frame %s hash not reproducible", res[i].Label)
		}
	}
}

func TestExportSparseKeyframeSeekAndLanding(t *testing.T) {
	dir := t.TempDir()
	exp := NewExporter(sampleStream(), sampleItems(), nil, dir, 7)
	// Target at 25.5s: last keyframe at or before is tick 20s (seq 20).
	res, errs := exp.Export([]FrameRequest{{AtMaster: 25*clock.MasterHz + clock.MasterHz/2, Label: "x"}})
	if len(errs) != 0 || len(res) != 1 {
		t.Fatalf("unexpected: %v %v", res, errs)
	}
	fr := res[0]
	if fr.KeyframeMaster != 20*clock.MasterHz || fr.KeyframeSeq != 20 {
		t.Fatalf("keyframe landing wrong: master=%d seq=%d", fr.KeyframeMaster, fr.KeyframeSeq)
	}
	// Seek delta must be reported and non-negative (seek backward).
	if fr.SeekDeltaMs != 5500 {
		t.Fatalf("seek delta ms wrong: %d", fr.SeekDeltaMs)
	}
}

func TestExportKeyframeSeekFailureRecorded(t *testing.T) {
	dir := t.TempDir()
	// Items start at tick 10s, so a request at 5s has no preceding keyframe.
	items := []timeline.Item{{MasterPTS: 10 * clock.MasterHz, Keyframe: true, Seq: 10}}
	exp := NewExporter(sampleStream(), items, nil, dir, 1)
	_, errs := exp.Export([]FrameRequest{{AtMaster: 5 * clock.MasterHz, Label: "early"}})
	if len(errs) != 1 || errs[0].Code != model.ErrKeyframeSeek {
		t.Fatalf("want keyframe seek error, got %v", errs)
	}
}

func TestDefaultRequests(t *testing.T) {
	reqs := DefaultRequests(1800)
	if len(reqs) != 5 || reqs[0].Label != "head" || reqs[4].Label != "tail" {
		t.Fatalf("default requests wrong: %+v", reqs)
	}
	if reqs[0].AtMaster != 0 || reqs[2].AtMaster != 900*clock.MasterHz {
		t.Fatal("checkpoint ticks wrong")
	}
}

func TestHashMixDistributes(t *testing.T) {
	seen := map[uint64]bool{}
	for i := uint64(0); i < 1000; i++ {
		seen[hashMix(i)] = true
	}
	if len(seen) < 999 {
		t.Fatalf("hashMix collided heavily: %d unique", len(seen))
	}
}
