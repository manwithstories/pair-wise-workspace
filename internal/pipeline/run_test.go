package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
	"archivecheck.local/archivecheck/internal/fsguard"
)

func setup(t *testing.T, kind string) (*fsguard.Root, string, fixture.Damaged) {
	t.Helper()
	dir := t.TempDir()
	fx, err := fixture.New(kind, 17)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, kind+".mkv"), fx.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsguard.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root, kind + ".mkv", fx
}

func runPipeline(t *testing.T, kind string, recovery bool, slowMS int) (*Report, error) {
	t.Helper()
	root, in, _ := setup(t, kind)
	return Run(context.Background(), Options{
		InputPath:      in,
		OutputPath:     kind + ".mp4",
		Recovery:       recovery,
		QueueCapacity:  4,
		SlowConsumerMS: slowMS,
		Root:           root,
	})
}

func TestRunCleanProducesVerifiedOutput(t *testing.T) {
	rep, err := runPipeline(t, fixture.DamageClean, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rates.RoundtripMatch != 100 || rep.Rates.SegmentHashMatch != 100 {
		t.Fatalf("rates=%+v", rep.Rates)
	}
	if rep.Queue.Peak > rep.Queue.Capacity {
		t.Fatalf("peak %d > capacity %d", rep.Queue.Peak, rep.Queue.Capacity)
	}
	if rep.OutputSHA256 == "" || rep.OutputSize <= 0 {
		t.Fatalf("output not recorded: %+v", rep)
	}
	info, err := os.Stat(rep.OutputPath)
	if err != nil || info.Size() != rep.OutputSize {
		t.Fatalf("output file mismatch: %v", err)
	}
	if len(rep.Index.Entries) == 0 {
		t.Fatal("no keyframe index entries")
	}
	for _, e := range rep.Index.Entries {
		got, ok := rep.Index.AtOrBefore(e.Track, e.Tick)
		if !ok || got.OutputSample != e.OutputSample {
			t.Fatalf("index miss at track %d tick %d", e.Track, e.Tick)
		}
	}
}

func TestRunIsDeterministic(t *testing.T) {
	first, err := runPipeline(t, fixture.DamageClean, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runPipeline(t, fixture.DamageClean, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.MappingDigest != second.MappingDigest {
		t.Fatalf("digest differs: %s vs %s", first.MappingDigest, second.MappingDigest)
	}
	if first.OutputSHA256 != second.OutputSHA256 {
		t.Fatalf("output differs between identical runs")
	}
}

func TestRunSlowConsumerKeepsQueueBounded(t *testing.T) {
	rep, err := runPipeline(t, fixture.DamageSlowCancel, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queue.Peak > 4 {
		t.Fatalf("peak queue %d exceeded capacity 4", rep.Queue.Peak)
	}
	if rep.Rates.RoundtripMatch != 100 {
		t.Fatalf("slow consumer lost data: %+v", rep.Rates)
	}
}

func TestRunCancelledContext(t *testing.T) {
	root, in, _ := setup(t, fixture.DamageSlowCancel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Options{
		InputPath: in, OutputPath: "out.mp4", QueueCapacity: 2, Root: root,
	})
	if err == nil {
		t.Fatal("cancelled run must fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root.Dir, "out.mp4")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("cancelled run left an output file")
	}
}

func TestRunTruncatedFailsWithoutRecovery(t *testing.T) {
	_, err := runPipeline(t, fixture.DamageTruncated, false, 0)
	if err == nil {
		t.Fatal("truncated input must fail")
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", fault.ExitCode(err))
	}
	if fault.OffsetOf(err) <= 0 {
		t.Fatalf("no source offset: %v", err)
	}
}

func TestRunTruncatedRecoveryWritesOutput(t *testing.T) {
	rep, err := runPipeline(t, fixture.DamageTruncated, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) == 0 || rep.SkippedBytes <= 0 {
		t.Fatalf("skips not reported: %+v", rep.Skipped)
	}
	if rep.Rates.RoundtripMatch != 100 {
		t.Fatalf("recovered output is not faithful: %+v", rep.Rates)
	}
	if _, err := os.Stat(rep.OutputPath); err != nil {
		t.Fatalf("recovered output missing: %v", err)
	}
}

func TestRunOversizedNeverRecovers(t *testing.T) {
	_, err := runPipeline(t, fixture.DamageOversized, true, 0)
	if err == nil {
		t.Fatal("ambiguous boundary must abort")
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", fault.ExitCode(err))
	}
}

func TestRunUnknownSizeAndNoCues(t *testing.T) {
	for _, kind := range []string{fixture.DamageUnknownSize, fixture.DamageNoCues, fixture.DamageOutOfOrder} {
		rep, err := runPipeline(t, kind, false, 0)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if rep.Rates.RoundtripMatch != 100 {
			t.Fatalf("%s rates=%+v", kind, rep.Rates)
		}
	}
}

func TestRunRejectsPathEscape(t *testing.T) {
	root, _, _ := setup(t, fixture.DamageClean)
	_, err := Run(context.Background(), Options{
		InputPath: "../outside.mkv", OutputPath: "out.mp4", QueueCapacity: 2, Root: root,
	})
	if !errors.Is(err, fsguard.ErrEscape) {
		t.Fatalf("got %v", err)
	}
}

func TestRunRequiresQueueCapacity(t *testing.T) {
	root, in, _ := setup(t, fixture.DamageClean)
	if _, err := Run(context.Background(), Options{InputPath: in, OutputPath: "o.mp4", Root: root}); err == nil {
		t.Fatal("zero capacity accepted")
	}
}

func TestRunRefusesUnknownCodecWhenAsked(t *testing.T) {
	dir := t.TempDir()
	tracks := []fixture.TrackSpec{
		{Number: 1, Type: 1, CodecID: "V_MPEG2", Width: 320, Height: 180},
		{Number: 2, Type: 1, CodecID: "V_VP9", Width: 320, Height: 180},
	}
	blocks := []fixture.BlockSpec{
		{Track: 1, Tick: 0, Key: true, Data: fixture.Pattern("m", 0, 32)},
		{Track: 2, Tick: 0, Key: true, Data: fixture.Pattern("v", 0, 32)},
	}
	doc := fixture.Build(tracks, blocks, fixture.Layout{ClusterSpan: 4})
	if err := os.WriteFile(filepath.Join(dir, "mixed.mkv"), doc.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := fsguard.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), Options{
		InputPath: "mixed.mkv", OutputPath: "out.mp4", QueueCapacity: 2, Root: root,
		RefuseUnknownCodecs: true,
	})
	if !errors.Is(err, fault.ErrUnsupportedCodec) {
		t.Fatalf("got %v", err)
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", fault.ExitCode(err))
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out.mp4")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("refused input still produced an output")
	}

	// Without the flag the supported track is remuxed and the unknown track is
	// counted, never silently mapped.
	rep, err := Run(context.Background(), Options{
		InputPath: "mixed.mkv", OutputPath: "out2.mp4", QueueCapacity: 2, Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unknown) != 1 || rep.Unknown[0] != "V_MPEG2" {
		t.Fatalf("unknown=%v", rep.Unknown)
	}
	if len(rep.Tracks) != 1 || rep.Tracks[0].CodecID != "V_VP9" {
		t.Fatalf("tracks=%+v", rep.Tracks)
	}
}

func TestRunWithRTPCheck(t *testing.T) {
	root, in, _ := setup(t, fixture.DamageClean)
	rep, err := Run(context.Background(), Options{
		InputPath: in, OutputPath: "rtp.mp4", QueueCapacity: 4, Root: root, RTPCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RTPPackets == 0 {
		t.Fatal("RTP validation produced no packets")
	}
	if rep.Rates.RoundtripMatch != 100 {
		t.Fatalf("RTP check disturbed the remux: %+v", rep.Rates)
	}
}
