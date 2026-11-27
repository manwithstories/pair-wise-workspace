package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avpipe/internal/graph"
	"avpipe/internal/media"
	"avpipe/internal/wav"
)

func testOptions(t *testing.T) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.Out = dir
	opts.Frames = 24000 // 0.5 s keeps the suite fast
	opts.ConsumerStallMS = 2
	return opts, dir
}

func TestFixturesAreDeterministicAndValid(t *testing.T) {
	dir := t.TempDir()
	a, err := GenerateFixtures(filepath.Join(dir, "a"), 42, 4800, 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateFixtures(filepath.Join(dir, "b"), 42, 4800, 7)
	if err != nil {
		t.Fatal(err)
	}
	da, _ := os.ReadFile(a.Normal)
	db, _ := os.ReadFile(b.Normal)
	if string(da) != string(db) {
		t.Fatal("identical seeds produced different fixtures")
	}
	c, _ := GenerateFixtures(filepath.Join(dir, "c"), 43, 4800, 7)
	dc, _ := os.ReadFile(c.Normal)
	if string(da) == string(dc) {
		t.Fatal("different seeds produced identical fixtures")
	}

	// The normal fixture must parse as 6-channel PCM16 with the declared length.
	r, err := wav.Open(a.Normal, wav.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if info := r.Info(); info.Channels != 6 || info.BitDepth != 16 || info.Frames != 4800 {
		t.Fatalf("info=%+v", info)
	}
	// The truncated fixture must be exactly 7 bytes shorter and declared longer.
	sn, _ := os.Stat(a.Normal)
	st, _ := os.Stat(a.Truncated)
	if sn.Size()-st.Size() != 7 {
		t.Fatalf("truncation lost %d bytes", sn.Size()-st.Size())
	}
	if len(a.Anchors) == 0 {
		t.Fatal("no anchors generated")
	}
}

func TestFixtureValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateFixtures(dir, 1, 0, 0); err == nil {
		t.Fatal("zero frames accepted")
	}
	if _, err := GenerateFixtures(dir, 1, 10, 1000); err == nil {
		t.Fatal("oversize truncation accepted")
	}
	if _, err := GenerateFixtures(dir, 1, 10, -1); err == nil {
		t.Fatal("negative truncation accepted")
	}
}

func TestOptionsValidate(t *testing.T) {
	base := DefaultOptions()
	base.Out = t.TempDir()
	if err := base.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	bad := []Options{
		{Queue: 0, Out: base.Out, Frames: 10},
		{Queue: 1, Out: base.Out, Frames: 10, ConsumerStallMS: -1},
		{Queue: 1, Out: base.Out, Frames: 10, CorruptTailBytes: -1},
		{Queue: 1, Out: base.Out, Frames: 0},
		{Queue: 1, Frames: 10},
		{Queue: 1, Out: base.Out, Frames: 10, CorruptTailBytes: 1000},
	}
	for i, o := range bad {
		if err := o.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if media.ExitFor(o.Validate()) != media.ExitUsage {
			t.Fatalf("case %d exit", i)
		}
	}
}

func TestEndToEndReport(t *testing.T) {
	opts, dir := testOptions(t)
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	rep, err := Run(context.Background(), opts, devnull, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed() {
		t.Fatalf("report failed: %+v", rep)
	}
	if rep.RoundTripPct != 100 {
		t.Fatalf("roundtrip=%f", rep.RoundTripPct)
	}
	if rep.KeyframePct != 100 {
		t.Fatalf("keyframe=%f", rep.KeyframePct)
	}
	if rep.SegmentCount == 0 || len(rep.SegmentSHA) != rep.SegmentCount {
		t.Fatalf("segments=%d hashes=%d", rep.SegmentCount, len(rep.SegmentSHA))
	}
	if rep.QueueCapacity != opts.Queue {
		t.Fatalf("capacity=%d", rep.QueueCapacity)
	}
	if rep.QueueHighWater > rep.QueueCapacity {
		t.Fatalf("high water=%d > capacity", rep.QueueHighWater)
	}
	if rep.Backpressure == 0 || !rep.QueueFullSeen {
		t.Fatal("no backpressure observed")
	}
	if rep.RecoveredBytes != int64(opts.CorruptTailBytes) {
		t.Fatalf("recovered=%d want %d", rep.RecoveredBytes, opts.CorruptTailBytes)
	}
	if rep.RepeatMatch != 100 {
		t.Fatalf("repeat=%f", rep.RepeatMatch)
	}
	if !rep.StrictRejected {
		t.Fatal("strict pass did not reject the truncated input")
	}
	if rep.PeakDBFS > -1 {
		t.Fatalf("peak=%f", rep.PeakDBFS)
	}
	if rep.LoudnessDBFS < -21 || rep.LoudnessDBFS > -19 {
		t.Fatalf("loudness=%f", rep.LoudnessDBFS)
	}

	// The atomic install must leave a valid WAV and no temporary file.
	if _, err := os.Stat(filepath.Join(dir, "session.wav.tmp-deterministic")); !os.IsNotExist(err) {
		t.Fatalf("temporary output left behind: %v", err)
	}
	out, err := wav.Open(filepath.Join(dir, "session.wav"), wav.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if info := out.Info(); info.Channels != 2 || info.BitDepth != 16 {
		t.Fatalf("output format=%+v", info)
	}
}

func TestReportFailsWhenCriteriaUnmet(t *testing.T) {
	ok := Report{
		RoundTripPct: 100, KeyframePct: 100, SegmentCount: 1, SegmentSHA: []uint64{1},
		QueueCapacity: 4, QueueHighWater: 4, QueueFullSeen: true, Backpressure: 1, RecoveredBytes: 7,
		StrictRejected: true, RepeatEnabled: true, RepeatMatch: 100,
		PeakDBFS: -3, LoudnessDBFS: -20,
	}
	if !ok.Passed() {
		t.Fatal("baseline should pass")
	}
	cases := map[string]func(*Report){
		"roundtrip":  func(r *Report) { r.RoundTripPct = 99.9 },
		"keyframe":   func(r *Report) { r.KeyframePct = 99 },
		"segments":   func(r *Report) { r.SegmentCount = 0 },
		"hash len":   func(r *Report) { r.SegmentSHA = nil },
		"bound":      func(r *Report) { r.QueueHighWater = 5 },
		"capacity":   func(r *Report) { r.QueueCapacity = 0 },
		"backpr":     func(r *Report) { r.Backpressure = 0 },
		"never full": func(r *Report) { r.QueueFullSeen = false },
		"recover":    func(r *Report) { r.RecoveredBytes = 0 },
		"strict":     func(r *Report) { r.StrictRejected = false },
		"repeat":     func(r *Report) { r.RepeatMatch = 99 },
		"peak":       func(r *Report) { r.PeakDBFS = -0.5 },
		"loud hi":    func(r *Report) { r.LoudnessDBFS = -18.9 },
		"loud lo":    func(r *Report) { r.LoudnessDBFS = -21.1 },
	}
	for name, mutate := range cases {
		r := ok
		mutate(&r)
		if r.Passed() {
			t.Fatalf("%s: still passed", name)
		}
	}
}

// TestGraphFailureHasNoOutputSideEffects proves dependency cycles, missing
// tracks and out-of-range anchor spans are rejected before the output file is
// created.
func TestGraphFailureHasNoOutputSideEffects(t *testing.T) {
	cases := map[string]func(int) []graph.Node{
		"cycle": func(anchors int) []graph.Node {
			return []graph.Node{
				{ID: "a", Stage: graph.StageSource, Track: "program", Deps: []string{"c"}},
				{ID: "b", Stage: graph.StageSink, Track: "program", Deps: []string{"a"}},
				{ID: "c", Stage: graph.StageAudit, Track: "program", Deps: []string{"b"}},
			}
		},
		"missing track": func(anchors int) []graph.Node {
			return []graph.Node{{ID: "a", Stage: graph.StageSource, Track: "ghost"}}
		},
		"span": func(anchors int) []graph.Node {
			return []graph.Node{{ID: "a", Stage: graph.StageAudit, Anchors: &graph.Span{Lo: 0, Hi: anchors + 1}}}
		},
	}
	for name, override := range cases {
		opts, dir := testOptions(t)
		opts.Frames = 4800
		opts.nodeOverride = override
		devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		_, err := Run(context.Background(), opts, devnull, os.Stderr)
		devnull.Close()
		if err == nil {
			t.Fatalf("%s: graph error not reported", name)
		}
		if media.ExitFor(err) != media.ExitUsage {
			t.Fatalf("%s: exit=%d want 2", name, media.ExitFor(err))
		}
		if _, statErr := os.Stat(filepath.Join(dir, "session.wav")); !os.IsNotExist(statErr) {
			t.Fatalf("%s: output file exists despite failure", name)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "session.wav.tmp-deterministic")); !os.IsNotExist(statErr) {
			t.Fatalf("%s: temporary output exists despite failure", name)
		}
	}
}

func TestRunRejectsBadOptions(t *testing.T) {
	opts := DefaultOptions()
	opts.Out = t.TempDir()
	opts.Queue = 0
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	_, err := Run(context.Background(), opts, devnull, os.Stderr)
	if err == nil || media.ExitFor(err) != media.ExitUsage {
		t.Fatalf("err=%v", err)
	}
}

func TestKeyframesParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kf.txt")
	body := "# comment\n0 k0\n60000 k1\n\n120000 k2\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKeyframes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Label != "k1" {
		t.Fatalf("anchors=%+v", got)
	}
	bad := map[string]string{
		"garbage":  "abc k0\n",
		"missing":  "0\n",
		"negative": "-5 k0\n",
		"empty":    "",
		"too many": "0 a b\n",
	}
	for name, content := range bad {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".txt")
		os.WriteFile(p, []byte(content), 0o644)
		if _, err := LoadKeyframes(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := LoadKeyframes(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestGraphValidationRejectsBrokenPlan(t *testing.T) {
	fx := Fixtures{
		Rate:    media.Rate{Num: 48000, Den: 1},
		Labels:  []string{"M"},
		Anchors: []AnchorSpec{{At: 0, Label: "k0"}},
	}
	tl, err := buildTimeline(fx)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGraph(tl, 1); err != nil {
		t.Fatal(err)
	}
	// An empty anchor span is valid, but a negative one is not.
	if err := validateGraph(tl, 0); err != nil {
		t.Fatalf("empty span rejected: %v", err)
	}
	err = validateGraph(tl, -1)
	if err == nil {
		t.Fatal("negative anchor count accepted")
	}
	if media.ExitFor(err) != media.ExitUsage {
		t.Fatal("graph failure should be a usage error")
	}
}
