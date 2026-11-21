package probe

import (
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
)

func baseOpts() fixture.Options {
	return fixture.Options{
		Seed: 42, DurationSeconds: 4, VideoTimescale: 90000, VideoFPS: 25,
		AudioTimescale: 48000, AudioDelta: 9600, KeyEvery: 25,
		StartDTS: 90000, EditList: true, VideoBody: 16, AudioBody: 8,
		VideoStreamID: fixture.StreamVideo(), AudioStreamID: fixture.StreamAudio(),
	}
}

func parseMem(t *testing.T, m *fixture.MemFile) *Result {
	t.Helper()
	res, err := Parse(m.ReaderAt(), int64(m.Len()), "test.mp4")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return res
}

func hasCode(errs []*model.MediaError, code model.ErrorCode) bool {
	for _, e := range errs {
		if e.Code == code {
			return true
		}
	}
	return false
}

func TestParseHealthyUnifiedTimeline(t *testing.T) {
	m, _, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(res.Tracks))
	}
	var video, audio *Track
	for _, tr := range res.Tracks {
		if tr.IsVideo {
			video = tr
		} else {
			audio = tr
		}
	}
	if video == nil || audio == nil {
		t.Fatal("missing video or audio track")
	}
	if video.Timescale != 90000 || len(video.Samples) != 100 {
		t.Fatalf("video samples = %d", len(video.Samples))
	}
	// Nonzero start DTS must surface on every sample.
	if video.StartDTS != 90000 {
		t.Fatalf("start DTS = %d, want 90000", video.StartDTS)
	}
	if video.Samples[0].StartTicks != 0 {
		t.Fatalf("edited first PTS tick = %d, want 0", video.Samples[0].StartTicks)
	}
	// First frame duration on the 90k grid.
	if d := video.Samples[0].EndTicks - video.Samples[0].StartTicks; d != 3600 {
		t.Fatalf("frame ticks = %d, want 3600", d)
	}
	if len(res.MediaErrors) != 0 {
		t.Fatalf("healthy asset produced defects: %v", res.MediaErrors)
	}
}

func TestEmptyEditList(t *testing.T) {
	o := baseOpts()
	o.EditList = false
	o.StartDTS = 0
	m, _, err := fixture.GenerateFile(o)
	if err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	for _, tr := range res.Tracks {
		if len(tr.Edits) != 0 {
			t.Fatalf("track %d unexpectedly has edits", tr.ID)
		}
	}
}

func TestTruncatedTail(t *testing.T) {
	m, lay, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.TruncateTailFile(m, lay, 40); err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	if !hasCode(res.MediaErrors, model.CodeContainerTruncated) {
		t.Fatalf("truncation not detected: %v", res.MediaErrors)
	}
}

func TestForgedLengthOutOfBounds(t *testing.T) {
	m, lay, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.ForgeOversizeSampleFile(m, lay); err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	if !hasCode(res.MediaErrors, model.CodeLengthOutOfBounds) {
		t.Fatalf("forged length not detected: %v", res.MediaErrors)
	}
}

func TestIndexMissingPage(t *testing.T) {
	m, lay, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.BloatSTCOCountFile(m, lay); err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	if !res.Integrity.IndexIncomplete {
		t.Fatal("bloated stco must mark index incomplete")
	}
}

func TestSparseKeyframeBoundaryAndGap(t *testing.T) {
	o := baseOpts()
	o.KeyEvery = 50 // keyframes every 2 s; 6 s cuts force gaps on windows
	m, _, err := fixture.GenerateFile(o)
	if err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	var video *Track
	for _, tr := range res.Tracks {
		if tr.IsVideo {
			video = tr
		}
	}
	// Window smaller than the 2 s keyframe spacing around a midpoint cut.
	plans := []CutPlan{{At: 90000}, {At: 270000}}
	bounds := ScanBoundaries(video, plans, 45000)
	gapCount := 0
	for _, b := range bounds {
		if b.Gap != nil {
			gapCount++
			if b.Gap.End <= b.Gap.Start {
				t.Fatal("gap must carry a positive tick span")
			}
		}
	}
	if gapCount == 0 {
		t.Fatal("sparse keyframes must produce at least one explicit gap")
	}
}

func TestCorruptSampleDetectedByCRC(t *testing.T) {
	m, lay, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.CorruptSampleFile(m, lay, 10); err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	var video *Track
	for _, tr := range res.Tracks {
		if tr.IsVideo {
			video = tr
		}
	}
	invalid, errs := VerifySamples(m.ReaderAt(), res, video)
	if _, bad := invalid[10]; !bad {
		t.Fatal("CRC-corrupted sample 10 not flagged")
	}
	if !hasCode(errs, model.CodeCorruptSample) {
		t.Fatalf("missing corrupt_sample code: %v", errs)
	}
}

func TestRecoverablePrefix(t *testing.T) {
	m, _, err := fixture.GenerateFile(baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	res := parseMem(t, m)
	var video *Track
	for _, tr := range res.Tracks {
		if tr.IsVideo {
			video = tr
		}
	}
	invalid := map[uint32]struct{}{60: {}}
	prefix := RecoverablePrefix(video, invalid)
	if prefix.To != 60 {
		t.Fatalf("prefix ends at %d, want 60", prefix.To)
	}
	tail := QuarantineTail(video, prefix)
	if tail == nil || tail.Start >= tail.End {
		t.Fatal("quarantine tail missing or empty")
	}
}
