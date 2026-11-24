package index

import (
	"bytes"
	"errors"
	"testing"

	"review/internal/ebml"
	"review/internal/fixture"
	"review/internal/media"
	"review/internal/mkv"
	"review/internal/timeline"
)

func studioTimeline(t *testing.T) *timeline.Timeline {
	t.Helper()
	doc, err := mkv.Decode(ebml.NewReader(
		bytes.NewReader(fixture.StudioInterview(42).Build())))
	if err != nil {
		t.Fatal(err)
	}
	tl, err := timeline.Merge([]*mkv.Document{doc}, timeline.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func TestBuildIndexesKeyframes(t *testing.T) {
	tl := studioTimeline(t)
	ix, err := Build(1, tl.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Entries) == 0 || len(ix.Keyframes) == 0 {
		t.Fatal("index or keyframe set empty")
	}
	if ix.Keyframes[0].Sample != 1 {
		t.Fatalf("first keyframe sample = %d, want 1", ix.Keyframes[0].Sample)
	}
	// Entries are strictly ordered in time with contiguous sample numbers.
	for i, e := range ix.Entries {
		if e.Sample != uint32(i+1) {
			t.Fatalf("sample numbering gap at %d: %d", i, e.Sample)
		}
		if i > 0 && e.Time < ix.Entries[i-1].Time {
			t.Fatal("index not time ordered")
		}
	}
}

func TestBuildRejectsEmptyAndKeylessTrack(t *testing.T) {
	tl := studioTimeline(t)
	if _, err := Build(999, tl.Blocks); err == nil {
		t.Fatal("missing track must error")
	}
	// Strip keyframe flags.
	stripped := make([]mkv.Block, len(tl.Blocks))
	copy(stripped, tl.Blocks)
	for i := range stripped {
		stripped[i].Keyframe = false
	}
	var pe *media.PipelineError
	if _, err := Build(1, stripped); !errors.As(err, &pe) {
		t.Fatalf("keyless track must pipeline-error, got %v", err)
	}
}

func TestNearestKeyframeAndHitRate(t *testing.T) {
	tl := studioTimeline(t)
	ix, err := Build(1, tl.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := ix.NearestKeyframe(ix.Keyframes[0].Time)
	if err != nil || anchor.Sample != ix.Keyframes[0].Sample {
		t.Fatalf("nearest keyframe: %+v err=%v", anchor, err)
	}
	if _, err := ix.NearestKeyframe(media.FromNanoseconds(-1)); err == nil {
		t.Fatal("timestamp before first keyframe must error")
	}
	var wanted []media.MediaTime
	for _, k := range ix.Keyframes {
		wanted = append(wanted, k.Time)
	}
	if rate := ix.KeyframeHitRate(wanted, 0); rate != 1.0 {
		t.Fatalf("exact-match hit rate = %v, want 1.0", rate)
	}
	tolerance := media.FromMilli(1)
	slightlyOff := make([]media.MediaTime, len(wanted))
	for i, w := range wanted {
		slightlyOff[i] = w + 1
	}
	if rate := ix.KeyframeHitRate(slightlyOff, tolerance); rate != 1.0 {
		t.Fatalf("tolerant hit rate = %v, want 1.0", rate)
	}
}

func TestPreviewRowsProjection(t *testing.T) {
	tl := studioTimeline(t)
	ix, err := Build(1, tl.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	rows, delta, err := ix.PreviewRows(30000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(ix.Entries) || delta == 0 {
		t.Fatalf("preview projection wrong: rows=%d delta=%d", len(rows), delta)
	}
	if !rows[0].Keyframe {
		t.Fatal("first preview row must be a keyframe")
	}
	// Deterministic re-projection.
	again, _, err := ix.PreviewRows(30000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i] != again[i] {
			t.Fatal("preview projection is not deterministic")
		}
	}
}

// TestParseErrorReachesIndex verifies the required propagation path: a
// truncated EBML stream fails decode, and the index builder never sees a
// partial block list masquerading as complete.
func TestParseErrorReachesIndex(t *testing.T) {
	raw := fixture.TruncateTail(fixture.StudioInterview(42))
	_, err := mkv.Decode(ebml.NewReader(bytes.NewReader(raw)))
	if err == nil {
		t.Fatal("truncated fixture decoded cleanly")
	}
	var te *ebml.TruncatedError
	if !errors.As(err, &te) {
		t.Fatalf("index input must carry truncation, got %v", err)
	}
	// The DAG contract: on parse failure the index node wraps the error
	// rather than building anything.
	wrapped := media.BlockedBy("keyframe-index",
		media.NewBoundary("mkv-decode", "truncated stream", 0, te.MissingOffset, te))
	if media.FirstBlocker(wrapped) != "mkv-decode" {
		t.Fatalf("blocker chain lost: %v", wrapped)
	}
}
