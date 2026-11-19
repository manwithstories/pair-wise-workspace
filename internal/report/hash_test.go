package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
	"archivetl/internal/pipeline"
	"archivetl/internal/subtitle"
	"archivetl/internal/timeline"
)

func testSegment(id int) *pipeline.Segment {
	seg := &pipeline.Segment{
		ID: id, Start: clock.MillisToTick(int64(id * 3000)),
		End:       clock.MillisToTick(int64(id*3000 + 3000)),
		InputHash: "input-" + strings.Repeat("0", 55),
		Records: []subtitle.Record{{
			Track: "t", Source: "in.srt", Sequence: 1, Offset: 12,
			Start: clock.MillisToTick(100), End: clock.MillisToTick(200), Text: "x",
		}},
	}
	items := []timeline.Item{{Track: "t", Sequence: 1,
		Start: seg.Records[0].Start, End: seg.Records[0].End, Text: "x"}}
	lanes, summary := timeline.Merge(items, clock.MillisToTick(60000))
	seg.Lanes, seg.Summary = lanes, summary
	seg.FailedOffsets = []int64{12}
	return seg
}

func TestStoreWriteAndVerify(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seg := testSegment(1)
	payload := []byte("deterministic-frame-bytes")
	seg.OutputHash = hashBytes(payload)
	if err := store.WriteSegment(seg, payload); err != nil {
		t.Fatal(err)
	}
	bad, err := store.Verify()
	if err != nil || len(bad) != 0 {
		t.Fatalf("fresh verify bad=%v err=%v", bad, err)
	}
	// Reopen: manifest must survive and resume match succeed.
	reopened, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	match, err := reopened.HasMatchingOutput(1, seg.InputHash)
	if err != nil || !match {
		t.Fatalf("resume match=%v err=%v", match, err)
	}
	// Different input fingerprint forces recomputation.
	match, _ = reopened.HasMatchingOutput(1, "changed")
	if match {
		t.Fatal("changed input hash incorrectly matched")
	}
}

func TestTamperDetected(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	seg := testSegment(2)
	payload := []byte("payload-bytes-here")
	seg.OutputHash = hashBytes(payload)
	if err := store.WriteSegment(seg, payload); err != nil {
		t.Fatal(err)
	}
	path := store.segmentPath(2)
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := store.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0] != 2 {
		t.Fatalf("tamper not detected: %v", bad)
	}
	match, err := store.HasMatchingOutput(2, seg.InputHash)
	if err != nil || match {
		t.Fatalf("tampered file matched=%v err=%v", match, err)
	}
}

func TestMissingFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	seg := testSegment(3)
	payload := []byte("abc")
	seg.OutputHash = hashBytes(payload)
	if err := store.WriteSegment(seg, payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.segmentPath(3)); err != nil {
		t.Fatal(err)
	}
	match, err := store.HasMatchingOutput(3, seg.InputHash)
	if err != nil || match {
		t.Fatalf("missing file matched=%v err=%v", match, err)
	}
}

func TestSummaryJSON(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewFileStore(dir)
	seg := testSegment(4)
	payload := []byte("frame")
	seg.OutputHash = hashBytes(payload)
	if err := store.WriteSegment(seg, payload); err != nil {
		t.Fatal(err)
	}
	coll := diag.NewCollector(0)
	coll.Add(diag.New(diag.PhaseParse, "in.srt", "cue.end", "boom", true).At("in.srt", 77))
	results := []pipeline.Result{{Segment: seg}}
	b := NewBuilder(store)
	sum := b.Build(results, coll, 8, 10, 5, 5)
	raw, err := sum.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("summary not valid JSON: %v", err)
	}
	if _, ok := decoded["segment_sha256"]; !ok {
		t.Fatal("segment_sha256 field missing")
	}
	if decoded["keyframe_hit_rate"].(float64) != 0.8 {
		t.Fatalf("hit rate=%v", decoded["keyframe_hit_rate"])
	}
	if decoded["roundtrip_rate"].(float64) != 1.0 {
		t.Fatalf("roundtrip=%v", decoded["roundtrip_rate"])
	}
	errs := decoded["errors"].([]any)
	first := errs[0].(map[string]any)
	if first["offset"].(float64) != 77 || first["source"] != "in.srt" {
		t.Fatalf("error fields wrong: %v", first)
	}
	segs := decoded["segments"].([]any)
	if len(segs) != 1 || len(segs[0].(map[string]any)["segment_sha256"].(string)) != 64 {
		t.Fatal("per-segment sha256 missing or wrong length")
	}
	if !filepath.IsAbs(filepath.Join(dir, "manifest.json")) {
		t.Fatal("sanity")
	}
}
