package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/audio"
	"mediamerge/internal/fault"
	"mediamerge/internal/journal"
	"mediamerge/internal/metadata"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions()
	opts.WorkDir = t.TempDir()
	opts.ConsumerDelayMs = 0
	opts.ChunkBytes = 4096
	opts.MemoryLimitBytes = 128 << 10
	return opts
}

func TestFixturesAreDeterministic(t *testing.T) {
	dir := t.TempDir()
	_, paths, err := WriteFixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range paths {
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := WriteFixtures(dir); err != nil {
			t.Fatal(err)
		}
		second, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Fatalf("%s is not byte-for-byte reproducible", name)
		}
	}
}

func TestRunMeetsAcceptanceAndIsRepeatable(t *testing.T) {
	opts := testOptions(t)
	first, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Parse.RoundtripRate() != 1 {
		t.Fatalf("roundtrip %.3f: %v", first.Parse.RoundtripRate(), first.Parse.Mismatches)
	}
	if first.Parse.KeyframeRate() != 1 {
		t.Fatalf("keyframe %.3f: %v", first.Parse.KeyframeRate(), first.Parse.ProbeMisses)
	}
	if first.SegmentHashes != first.SegmentHashesTotal || first.SegmentHashes < 8 {
		t.Fatalf("segment hashes %d/%d", first.SegmentHashes, first.SegmentHashesTotal)
	}
	if len(first.CommittedStages) != 4 {
		t.Fatalf("committed %v", first.CommittedStages)
	}
	if first.WALRecovered {
		t.Fatal("a clean run must not claim recovery")
	}

	second, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"parse", "audio", "mux", "final"} {
		if first.StageDigests[stage] == "" {
			t.Fatalf("stage %s has no digest", stage)
		}
		if first.StageDigests[stage] != second.StageDigests[stage] {
			t.Fatalf("stage %s digest changed: %s vs %s", stage, first.StageDigests[stage], second.StageDigests[stage])
		}
	}
	if len(first.SegmentDigests) != len(second.SegmentDigests) {
		t.Fatalf("segment count changed")
	}
	for i := range first.SegmentDigests {
		if first.SegmentDigests[i].Digest != second.SegmentDigests[i].Digest {
			t.Fatalf("segment %d hash changed across runs", i+1)
		}
	}
	if first.Queue.HighWater > first.Queue.Budget {
		t.Fatalf("queue high water %d over budget %d", first.Queue.HighWater, first.Queue.Budget)
	}
}

func TestRunExercisesBackpressure(t *testing.T) {
	opts := testOptions(t)
	opts.ChunkBytes = 64 << 10
	opts.MemoryLimitBytes = 128 << 10
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queue.Blocked == 0 {
		t.Fatalf("producer never blocked: %+v", res.Queue)
	}
	if res.Queue.HighWater > res.Queue.Budget {
		t.Fatalf("over budget: %+v", res.Queue)
	}
	if res.ConsumerBlocks != res.SegmentHashesTotal {
		t.Fatalf("consumer saw %d, producer emitted %d", res.ConsumerBlocks, res.SegmentHashesTotal)
	}
}

func TestRunInterruptAndResume(t *testing.T) {
	opts := testOptions(t)
	opts.FailAfterStage = 2
	first, err := Run(context.Background(), opts)
	if !IsInterrupted(err) {
		t.Fatalf("want interrupt, got %v", err)
	}
	if first.FailStage != "mux" {
		t.Fatalf("fail stage %q", first.FailStage)
	}
	if len(first.CommittedStages) != 2 {
		t.Fatalf("committed before crash %v", first.CommittedStages)
	}
	if fault.Classify(err) != fault.ClassRetryable {
		t.Fatalf("interrupt must be retryable, got %s", fault.Classify(err))
	}

	opts.FailAfterStage = 0
	opts.Resume = true
	second, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !second.WALRecovered {
		t.Fatal("resume must report WAL recovery")
	}
	if second.RolledBackBytes <= 0 {
		t.Fatalf("rolled back %d", second.RolledBackBytes)
	}
	if len(second.CommittedStages) < 2 {
		t.Fatalf("resume committed %v", second.CommittedStages)
	}
	for _, stage := range []string{"parse", "audio", "mux", "final"} {
		if second.StageDigests[stage] == "" {
			t.Fatalf("stage %s missing after resume", stage)
		}
	}
	// The recovered segment digests must equal a clean run's.
	clean := testOptions(t)
	third, err := Run(context.Background(), clean)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.SegmentDigests) != len(third.SegmentDigests) {
		t.Fatalf("segments after resume %d vs %d", len(second.SegmentDigests), len(third.SegmentDigests))
	}
	for i := range second.SegmentDigests {
		if second.SegmentDigests[i].Digest != third.SegmentDigests[i].Digest {
			t.Fatalf("segment %d digest differs after recovery", i+1)
		}
	}
	// Recovery is idempotent: a further resume rolls back nothing.
	opts.Resume = true
	again, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.RolledBackBytes != 0 {
		t.Fatalf("second resume rolled back %d", again.RolledBackBytes)
	}
}

func TestRunDetectsTamperedCommittedSegment(t *testing.T) {
	opts := testOptions(t)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// Corrupt a committed byte in the audio region of the output file.
	path := filepath.Join(opts.WorkDir, "delivery.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 64 {
		t.Fatalf("output too small: %d", len(data))
	}
	data[16] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Resume = true
	opts.FailAfterStage = 0
	_, err = Run(context.Background(), opts)
	if err == nil {
		t.Fatal("tampered committed output must be detected")
	}
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeChecksumMismatch {
		t.Fatalf("bad error %v", err)
	}
}

func TestRunRejectsBadOptions(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"chunk", func(o *Options) { o.ChunkBytes = 0 }},
		{"delay", func(o *Options) { o.ConsumerDelayMs = -1 }},
		{"fail", func(o *Options) { o.FailAfterStage = -1 }},
		{"budget", func(o *Options) { o.MemoryLimitBytes = 1 }},
		{"rate", func(o *Options) { o.TargetSampleRate = 0 }},
	}
	for _, c := range cases {
		opts := testOptions(t)
		c.mut(&opts)
		_, err := Run(context.Background(), opts)
		var fe *fault.Error
		if !errors.As(err, &fe) || fe.Code != fault.CodeInvalidArgument || fe.Stage != fault.StageArgs {
			t.Fatalf("%s: bad error %v", c.name, err)
		}
	}
}

func TestCrossStageRotationAnomaly(t *testing.T) {
	// A non-right-angle matrix must be rejected by the resolver, which is
	// downstream of the container parser: the fault travels from parse
	// into resolve and keeps track and span context.
	dir := t.TempDir()
	doc, err := metadata.BuildDocument(metadata.WriteOptions{
		Tracks: []metadata.WriteTrack{{
			Kind: metadata.KindVideo, Timescale: 25, Duration: 25, SampleDelta: 1, SampleCount: 25,
			Width: 640, Height: 360, Matrix: [9]int32{46341, 46341, 0, -46341, 46341, 0, 0, 0, 1 << 30},
			Color: metadata.ColorDesc{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rotated.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Inspect(path)
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want fault, got %v", err)
	}
	if fe.Code != fault.CodeRotationInvalid || fe.TrackID != 1 || fe.Span == nil {
		t.Fatalf("bad propagation %+v", fe)
	}
	if fe.Stage != fault.StageResolve {
		t.Fatalf("fault should surface at resolve, got %s", fe.Stage)
	}
}

func TestCrossStageAudioIndivisiblePropagates(t *testing.T) {
	// An audio frame count that cannot be resampled must fail inside the
	// audio stage with track and timebase context, not be silently rounded.
	tr := &metadata.Track{ID: 7, Kind: metadata.KindAudio, Timebase: metadataTimebase(), DurationTicks: 1,
		SampleRate: 48000, Channels: 2}
	_, err := audio.Resample(tr.ID, []int32{1, 2}, 2, 48000, 44100, tr.Timebase)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeSampleIndivisible || fe.TrackID != 7 {
		t.Fatalf("bad error %v", err)
	}
	if fe.Span == nil || fe.Span.End.IsZero() {
		t.Fatalf("missing span %+v", fe)
	}
}

func TestWriteCorruptFixturesAllFailInspect(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range CorruptNames() {
		path, err := WriteCorruptFixture(dir, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if _, err := Inspect(path); err == nil {
			t.Fatalf("%s: corrupt fixture parsed cleanly", kind)
		}
	}
	if _, err := WriteCorruptFixture(dir, "nonsense"); err == nil {
		t.Fatal("unknown corruption must fail")
	}
}

func TestVerifyReportsCommittedState(t *testing.T) {
	opts := testOptions(t)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(opts.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CommittedStages) != 4 {
		t.Fatalf("committed %v", res.CommittedStages)
	}
	if res.Parse.RoundtripRate() != 1 {
		t.Fatalf("roundtrip %.3f", res.Parse.RoundtripRate())
	}
	if _, err := Verify(t.TempDir()); err == nil {
		t.Fatal("empty directory must not verify")
	}
}

func TestJournalRecordCountMatchesStages(t *testing.T) {
	opts := testOptions(t)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(opts.WorkDir, "run.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, tail, err := journal.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if tail.TruncatedBytes != 0 || tail.TruncatedReason != "" {
		t.Fatalf("clean run left a torn tail: %+v", tail)
	}
	var begins, commits int
	for _, r := range records {
		switch r.Kind {
		case journal.KindBegin:
			begins++
		case journal.KindCommit:
			commits++
			if r.Summary == "" {
				t.Fatal("commit without a summary")
			}
		}
	}
	if begins != 4 || commits != 4 {
		t.Fatalf("begins %d commits %d", begins, commits)
	}
}

func TestSmallWindowResamplingIsExact(t *testing.T) {
	// A 24 kHz track converted to 48 kHz must produce identical audio no
	// matter how small the memory budget makes each window, otherwise the
	// streamed delivery would depend on the machine's memory.
	results := map[int64][]byte{}
	for _, budget := range []int64{32 << 10, 128 << 10, 512 << 10} {
		opts := DefaultOptions()
		opts.WorkDir = t.TempDir()
		opts.ConsumerDelayMs = 0
		opts.ChunkBytes = 4096
		opts.MemoryLimitBytes = budget
		res, err := Run(context.Background(), opts)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		var all []byte
		for _, seg := range res.SegmentDigests {
			all = append(all, seg.Digest[:]...)
		}
		results[budget] = all
	}
	var want []byte
	for budget, got := range results {
		if want == nil {
			want = got
			continue
		}
		if string(got) != string(want) {
			t.Fatalf("segment hashes depend on the memory budget (%d)", budget)
		}
	}
}
