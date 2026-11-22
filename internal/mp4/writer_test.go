package mp4

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"medialine/internal/fault"
	"medialine/internal/model"
	"medialine/internal/testkit"
)

func planFromClip(t *testing.T, clip *testkit.Clip) (SegmentPlan, [][]byte) {
	t.Helper()
	f, err := Parse(clip.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := f.Tracks[0]
	sr, err := f.OpenSampleReader(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	var plan SegmentPlan
	plan.ID = "seg-1"
	plan.TrackID = tr.ID
	plan.Kind = model.SegmentSimple
	plan.Timescale = tr.Timescale
	plan.Codec = "vide"
	plan.StartUS = 0
	plan.EndUS = 1_000_000
	for _, s := range tr.Samples {
		if s.PTSUS >= plan.EndUS {
			break
		}
		b, err := sr.ReadSample(s)
		if err != nil {
			t.Fatal(err)
		}
		plan.Samples = append(plan.Samples, s)
		plan.Payloads = append(plan.Payloads, append([]byte(nil), b...))
	}
	return plan, plan.Payloads
}

func TestWriteSegmentRoundTrip(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 3_000_000, 30))
	plan, payloads := planFromClip(t, c)
	dir := t.TempDir()
	out := filepath.Join(dir, "seg.mp4")
	n, err := WriteSegment(out, plan, WriterOptions{Root: dir, MaxPayload: 1 << 20})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != len(data) {
		t.Fatalf("size mismatch")
	}
	// Payloads must appear verbatim in order.
	cursor := 0
	for i, p := range payloads {
		idx := bytes.Index(data[cursor:], p)
		if idx < 0 {
			t.Fatalf("payload %d missing", i)
		}
		cursor += idx + len(p)
	}
	// The written file must parse back with matching sample tables.
	f, err := Parse(out, Options{})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(f.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(f.Tracks))
	}
	got := f.Tracks[0]
	if len(got.Samples) != len(plan.Samples) {
		t.Fatalf("samples=%d want %d", len(got.Samples), len(plan.Samples))
	}
	for i := range got.Samples {
		if got.Samples[i].Size != plan.Samples[i].Size {
			t.Fatalf("sample %d size %d want %d", i, got.Samples[i].Size, plan.Samples[i].Size)
		}
	}
}

func TestWriteSegmentDeterministic(t *testing.T) {
	c, _ := genClip(t, testkit.DefaultVideoTrack(1, 2_000_000, 15))
	plan, _ := planFromClip(t, c)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	if _, err := WriteSegment(a, plan, WriterOptions{Root: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSegment(b, plan, WriterOptions{Root: dir}); err != nil {
		t.Fatal(err)
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if !bytes.Equal(da, db) {
		t.Fatal("output not deterministic")
	}
}

func TestWriteSegmentRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seg.mp4")
	base := SegmentPlan{ID: "s", TrackID: 1, Timescale: 1000, StartUS: 0, EndUS: 1000}

	if _, err := WriteSegment(out, base, WriterOptions{Root: dir}); err == nil {
		t.Fatal("expected error for empty samples")
	}
	base.Timescale = 0
	base.Samples = []model.Sample{{Size: 1}}
	base.Payloads = [][]byte{{1}}
	if _, err := WriteSegment(out, base, WriterOptions{Root: dir}); err == nil {
		t.Fatal("expected error for zero timescale")
	}
	base.Timescale = 1000
	base.Samples[0].Size = 100
	if _, err := WriteSegment(out, base, WriterOptions{Root: dir}); err == nil {
		t.Fatal("expected mismatch error")
	}
	base.Samples[0].Size = 1 << 30
	base.Payloads[0] = make([]byte, 1<<30)
	if _, err := WriteSegment(out, base, WriterOptions{Root: dir, MaxPayload: 16}); err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestWriteSegmentPathEscape(t *testing.T) {
	plan := SegmentPlan{ID: "s", TrackID: 1, Timescale: 1000, Samples: []model.Sample{{Size: 1}}, Payloads: [][]byte{{0}}}
	dir := t.TempDir()
	if _, err := WriteSegment(filepath.Join(dir, "..", "escape.mp4"), plan, WriterOptions{Root: dir}); err == nil {
		t.Fatal("expected path escape error")
	}
	fe, ok := fault.As(mustErr(WriteSegment(filepath.Join(dir, "bad\x00.mp4"), plan, WriterOptions{Root: dir})))
	if !ok || fe.Kind != fault.KindPathEscape {
		t.Fatalf("expected path escape, got %v", fe)
	}
}

func mustErr(_ int64, err error) error { return err }

func TestCheckOutputPath(t *testing.T) {
	dir := t.TempDir()
	if err := checkOutputPath(dir, filepath.Join(dir, "a.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := checkOutputPath("", filepath.Join(dir, "a.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := checkOutputPath(dir, filepath.Join(dir, "..", "a.mp4")); err == nil {
		t.Fatal("expected escape")
	}
}
