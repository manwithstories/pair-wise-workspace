package index_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bcast/internal/fault"
	"bcast/internal/index"
	"bcast/internal/model"
	"bcast/internal/safe"
)

func testTimeline() *model.Timeline {
	return &model.Timeline{
		ID:            "nightly",
		FrameDuration: model.Rational{N: 1, D: 8},
		Duration:      model.Rational{N: 3, D: 1},
		Tracks: []model.Track{{
			ID: "V1", Kind: "video",
			Clips: []model.Clip{{
				ID: "a", Asset: "assets/a.mp4",
				Start: model.Rational{N: 0, D: 1},
				In:    model.Rational{N: 0, D: 1},
				Out:   model.Rational{N: 1, D: 1},
			}},
		}},
	}
}

func TestIndexKeyInvalidatesOnInputChange(t *testing.T) {
	tl := testTimeline()
	k1, err := index.NewKey(map[string]string{"a.mp4": "aaa"}, tl, map[string]int{"buffer": 2})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := index.NewKey(map[string]string{"a.mp4": "bbb"}, tl, map[string]int{"buffer": 2})
	if err != nil {
		t.Fatal(err)
	}
	k3, err := index.NewKey(map[string]string{"a.mp4": "aaa"}, tl, map[string]int{"buffer": 4})
	if err != nil {
		t.Fatal(err)
	}
	if k1.Hash() == k2.Hash() || k1.Hash() == k3.Hash() {
		t.Fatal("index key must change when input digest or params change")
	}
	st := &index.Store{KeyHash: k1.Hash(), Key: k1}
	if !st.Matches(k1) || st.Matches(k2) {
		t.Fatal("store match/invalidation is incorrect")
	}
}

func TestScanIsolatesCorruptAssetAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A plausible ftyp header followed by impossible/truncated payload.
	bad := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 1}
	bad = append(bad, make([]byte, 64)...)
	if err := os.WriteFile(filepath.Join(dir, "assets", "bad.mp4"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := index.ScanInputs(root, []string{"assets/bad.mp4"})
	if err != nil {
		t.Fatalf("corrupt asset should be isolated, not abort scan: %v", err)
	}
	if len(res.Rejected) != 1 || len(res.Accepted) != 0 {
		t.Fatalf("accepted=%d rejected=%d, want 0/1", len(res.Accepted), len(res.Rejected))
	}
	if res.Rejected[0].ReasonCode == "" {
		t.Fatal("rejected asset must retain a layered reason code")
	}

	if _, err := index.ScanInputs(root, []string{"../outside.mp4"}); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

func TestMP4SampleAndKeyframeRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	root, err := safe.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		rel := "assets/" + name + ".mp4"
		path, err := root.Resolve(rel)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i",
			"testsrc=size=160x120:rate=8:duration=1",
			"-pix_fmt", "yuv420p", "-c:v", "libx264", "-g", "2",
			"-keyint_min", "2", "-sc_threshold", "0", "-bf", "0",
			"-force_key_frames", "expr:eq(n,0)+eq(n,2)+eq(n,4)+eq(n,6)",
			"-fflags", "+bitexact", "-flags:v", "+bitexact", "-y", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg failed: %v\n%s", err, out)
		}
	}

	res, err := index.ScanInputs(root, []string{
		"assets/a.mp4", "assets/b.mp4", "assets/c.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accepted) != 3 || len(res.Rejected) != 0 {
		t.Fatalf("accepted=%d rejected=%d", len(res.Accepted), len(res.Rejected))
	}
	if got := res.Store.CountSamples(); got != 24 {
		t.Fatalf("samples=%d want 24", got)
	}
	if got := res.Store.CountKeyframes(); got != 12 {
		t.Fatalf("keyframes=%d want 12", got)
	}
	if err := res.Store.Save(root, "state/index.json"); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := index.LoadStore(root, "state/index.json")
	if err != nil || !ok {
		t.Fatalf("load index ok=%v err=%v", ok, err)
	}
	if loaded.CountSamples() != 24 || loaded.CountKeyframes() != 12 {
		t.Fatalf("roundtrip samples=%d keyframes=%d", loaded.CountSamples(), loaded.CountKeyframes())
	}
}

func TestPTSRegressionIsLocatable(t *testing.T) {
	tr := &index.TrackIndex{Samples: []index.Sample{
		{Index: 1, PTS: 0},
		{Index: 2, PTS: 800},
		{Index: 3, PTS: 700},
	}}
	err := index.ValidatePTSMonotonic(tr, "assets/pts-bad.mp4")
	if err == nil || !fault.IsCode(err, fault.PTSRegression) {
		t.Fatalf("err=%v want PTS_REGRESSION", err)
	}
	fe, ok := fault.As(err)
	if !ok || !fe.HasSeg || fe.Segment != 2 {
		t.Fatalf("location=%+v want segment 2", fe)
	}
}
