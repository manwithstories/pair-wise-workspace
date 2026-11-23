package demux_test

import (
	"bytes"
	"errors"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
	"bcast/media"
)

func assets(t *testing.T) *fixture.Assets {
	t.Helper()
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return a
}

func parse(t *testing.T, b []byte, name string) (*demux.File, error) {
	t.Helper()
	return demux.Parse(bytes.NewReader(b), int64(len(b)), name)
}

func TestParseClean(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.Clean, "clean.mp4")
	if err != nil {
		t.Fatalf("clean parse: %v", err)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(f.Tracks))
	}
	vt, _ := f.TrackByKind(media.TrackVideo)
	at, _ := f.TrackByKind(media.TrackAudio)
	if vt.Timescale != 90000 || at.Timescale != 48000 {
		t.Fatalf("timescales %d/%d", vt.Timescale, at.Timescale)
	}
	if got := len(f.SamplesOf(1)); got != 360 {
		t.Fatalf("video samples=%d", got)
	}
	if got := len(f.SamplesOf(2)); got != 120 {
		t.Fatalf("audio samples=%d", got)
	}
	// Keyframes from stss at every segment start except segment 5.
	keys := 0
	for _, s := range f.SamplesOf(1) {
		if s.Keyframe {
			keys++
			if s.KeySource != media.KeySrcSTSS {
				t.Fatalf("key source=%q", s.KeySource)
			}
		}
	}
	if keys != 11 {
		t.Fatalf("keyframes=%d", keys)
	}
	// Source offsets must be monotonic within a track.
	prev := int64(-1)
	for _, s := range f.SamplesOf(1) {
		if s.Offset <= prev {
			t.Fatalf("offsets not monotonic at %d", s.Offset)
		}
		prev = s.Offset
	}
}

func TestParseTruncated(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.Truncated, "trunc.mp4")
	if err == nil {
		t.Fatal("expected recoverable truncation error")
	}
	var me *media.Error
	if !errors.As(err, &me) || me.Class != media.ClassRecoverable {
		t.Fatalf("class: %v", err)
	}
	if !errors.Is(err, media.ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
	if me.Offset < 0 {
		t.Fatal("first invalid offset not reported")
	}
	// Video fully salvaged; audio tail dropped.
	if got := len(f.SamplesOf(1)); got != 360 {
		t.Fatalf("video samples=%d", got)
	}
	if got := len(f.SamplesOf(2)); got >= 120 || got == 0 {
		t.Fatalf("audio samples=%d", got)
	}
	if len(f.Damaged) == 0 {
		t.Fatal("no damaged range recorded")
	}
}

func TestParseBrokenBox(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.BrokenBox, "broken.mp4")
	if err == nil || !errors.Is(err, media.ErrBrokenBox) {
		t.Fatalf("want ErrBrokenBox, got %v", err)
	}
	if media.ClassOf(err) != media.ClassRecoverable {
		t.Fatalf("class: %v", err)
	}
	// All samples precede the broken box and survive.
	if got := len(f.Samples); got != 480 {
		t.Fatalf("samples=%d", got)
	}
	if len(f.Damaged) != 1 {
		t.Fatalf("damaged=%v", f.Damaged)
	}
	if !f.HasIssue(media.IssueBrokenBox) {
		t.Fatalf("issues=%+v", f.Issues)
	}
}

func TestMissingMoov(t *testing.T) {
	a := assets(t)
	// Cut before moov ends... instead: keep only ftyp+mdat tail.
	// Simpler: garbage input has no moov at all.
	_, err := parse(t, []byte("not an mp4 at all, no boxes here"), "junk")
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("want fatal, got %v", err)
	}
	_ = a
}

func TestMissingIndexRebuild(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.MissingIndex, "noidx.mp4")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, s := range f.SamplesOf(1) {
		if s.KeySource != media.KeySrcNone {
			t.Fatalf("unexpected key source %q", s.KeySource)
		}
	}
	if err := demux.BuildKeyIndex(f, bytes.NewReader(a.MissingIndex)); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	total, keys, src := demux.KeyIndexStats(f)
	if total != 360 || keys != 11 || src != media.KeySrcRebuilt {
		t.Fatalf("stats total=%d keys=%d src=%q", total, keys, src)
	}
}

func TestRTPSidecar(t *testing.T) {
	a := assets(t)
	b, err := demux.ParseRTPSidecar(bytes.NewReader(a.RTP))
	if err != nil {
		t.Fatalf("rtp: %v", err)
	}
	if b.Packets != 360 {
		t.Fatalf("packets=%d", b.Packets)
	}
	if b.FirstTimestamp != 0 || b.LastTimestamp != 359*3000 {
		t.Fatalf("bounds %d..%d", b.FirstTimestamp, b.LastTimestamp)
	}
	if b.ClockRateHint != 90000 {
		t.Fatalf("clock=%d", b.ClockRateHint)
	}
}

func TestIssueTaxonomySeparatesFaults(t *testing.T) {
	a := assets(t)
	cases := []struct {
		name string
		data []byte
		kind media.IssueKind
	}{
		{"truncated", a.Truncated, media.IssueTruncated},
		{"broken", a.BrokenBox, media.IssueBrokenBox},
		{"noidx", a.MissingIndex, media.IssueIncompleteIndex},
	}
	for _, c := range cases {
		f, _ := parse(t, c.data, c.name)
		if f.HasIssue(c.kind) {
			continue
		}
		// MissingIndex only becomes an issue once the index is requested.
		if c.kind == media.IssueIncompleteIndex {
			continue
		}
		t.Fatalf("%s: issues=%+v", c.name, f.Issues)
	}
	// The three damage classes must not collapse into one another.
	tr, _ := parse(t, a.Truncated, "t")
	if tr.HasIssue(media.IssueBrokenBox) {
		t.Fatal("truncation misclassified as broken box")
	}
	bb, _ := parse(t, a.BrokenBox, "b")
	if bb.HasIssue(media.IssueTruncated) {
		t.Fatal("broken box misclassified as truncation")
	}
	if _, err := parse(t, []byte("nope"), "x"); media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("missing moov class=%v", media.ClassOf(err))
	}
}

func TestCodecEntryPreserved(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.Clean, "clean")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "avc1", 2: "mp4a"}
	for _, tr := range f.Tracks {
		if len(tr.CodecEntry) < 8 {
			t.Fatalf("track %d codec entry len=%d", tr.ID, len(tr.CodecEntry))
		}
		if typ := string(tr.CodecEntry[4:8]); typ != want[tr.ID] {
			t.Fatalf("track %d entry type=%q want %q", tr.ID, typ, want[tr.ID])
		}
	}
}

func TestRebuildBlocksWithoutReadableSamples(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.MissingIndex, "noidx.mp4")
	if err != nil {
		t.Fatal(err)
	}
	// An empty reader stands in for a source that cannot supply payloads:
	// no keyframe can be recovered, so the index blocks downstream.
	err = demux.BuildKeyIndex(f, bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected blocked error")
	}
	if got := media.FirstBlockingDep(err); got != "demux.samples" {
		t.Fatalf("blocking dep=%q", got)
	}
	if media.ClassOf(err) != media.ClassBlocked {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
	if f.HasIssue(media.IssueIncompleteIndex) == false {
		t.Fatalf("issues=%+v", f.Issues)
	}
}

func TestRebuildRecordsSource(t *testing.T) {
	a := assets(t)
	f, err := parse(t, a.MissingIndex, "noidx.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := demux.BuildKeyIndex(f, bytes.NewReader(a.MissingIndex)); err != nil {
		t.Fatal(err)
	}
	if _, _, src := demux.KeyIndexStats(f); src != media.KeySrcRebuilt {
		t.Fatalf("source=%q", src)
	}
	// The clean file declares stss and must not be mislabelled as rebuilt.
	clean, err := parse(t, a.Clean, "clean")
	if err != nil {
		t.Fatal(err)
	}
	if err := demux.BuildKeyIndex(clean, bytes.NewReader(a.Clean)); err != nil {
		t.Fatal(err)
	}
	if _, _, src := demux.KeyIndexStats(clean); src != media.KeySrcSTSS {
		t.Fatalf("clean source=%q", src)
	}
}
