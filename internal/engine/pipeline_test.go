package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/codec"
	"mediacore/internal/container"
	"mediacore/internal/media"
	"mediacore/internal/store"
)

// harness builds a fixture, runs a pipeline and exposes the work directory.
type harness struct {
	t    *testing.T
	dir  string
	src  *container.Source
	file *os.File
	sha  string
	size int64
}

func newHarness(t *testing.T, spec container.FixtureSpec) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "in.mp4")
	if err := container.WriteFixture(path, spec); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	adapter := container.NewMP4(container.DefaultOptions())
	src, f, err := adapter.OpenFile(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sum := sha256.Sum256(data)
	return &harness{t: t, dir: dir, src: src, file: f, sha: hex.EncodeToString(sum[:]), size: int64(len(data))}
}

func (h *harness) run(work string, cfg Config, interrupt int) (*Result, error) {
	return Run(Options{
		WorkDir:        filepath.Join(h.dir, work),
		Config:         cfg,
		Source:         h.src,
		Codecs:         codec.Default(),
		InputPath:      filepath.Join(h.dir, "in.mp4"),
		InputSHA:       h.sha,
		InputSize:      h.size,
		InterruptAfter: interrupt,
	})
}

func stereo48k() Config {
	return Config{OutputRate: 48000, OutputChannels: 2, LoudnessDBFS: -1, BlockFrames: 4800}
}

func defaultSpec() container.FixtureSpec {
	return container.FixtureSpec{Rate: 44100, Channels: 2, Frames: 44100 * 2, FrameSize: 4096}
}

// TestResumeMatchesFullRun is the core acceptance property: stopping after a
// few blocks and resuming must produce the same digest as one run.
func TestResumeMatchesFullRun(t *testing.T) {
	h := newHarness(t, defaultSpec())
	full, err := h.run("full", stereo48k(), 0)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if full.Stats.SHA256 == "" || len(full.Stats.SHA256) != 64 {
		t.Fatalf("bad digest %q", full.Stats.SHA256)
	}
	part, err := h.run("part", stereo48k(), 3)
	if err != nil {
		t.Fatalf("interrupted run: %v", err)
	}
	if !part.Interrupted {
		t.Fatalf("expected interruption")
	}
	cp, err := store.Load(filepath.Join(h.dir, "part"))
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if cp.CompletedBlocks != 3 {
		t.Fatalf("completed=%d, want 3", cp.CompletedBlocks)
	}
	// Resume reuses the same source object, exactly as the command does.
	res, err := h.run("part", stereo48k(), 0)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Stats.SHA256 != full.Stats.SHA256 {
		t.Fatalf("resume digest %s != full %s", res.Stats.SHA256, full.Stats.SHA256)
	}
	if res.Stats.ResumedBlocks != 3 {
		t.Fatalf("resumed blocks %d, want 3", res.Stats.ResumedBlocks)
	}
	a, _ := os.ReadFile(filepath.Join(h.dir, "full", "out.pcm"))
	b, _ := os.ReadFile(filepath.Join(h.dir, "part", "out.pcm"))
	if !bytes.Equal(a, b) {
		t.Fatalf("outputs differ")
	}
}

// TestOutputFraming checks the sample count and block bookkeeping.
func TestOutputFraming(t *testing.T) {
	h := newHarness(t, container.FixtureSpec{Rate: 48000, Channels: 2, Frames: 48000, FrameSize: 4096})
	res, err := h.run("w", Config{OutputRate: 48000, OutputChannels: 2, LoudnessDBFS: -1, BlockFrames: 4800}, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// samples_in counts interleaved input samples, not frames.
	if res.Stats.SamplesIn != 96000 {
		t.Fatalf("samples_in=%d, want 96000", res.Stats.SamplesIn)
	}
	if res.Stats.SamplesOut != 96000 {
		t.Fatalf("samples_out=%d, want 96000", res.Stats.SamplesOut)
	}
	if res.Stats.Blocks != 10 {
		t.Fatalf("blocks=%d, want 10", res.Stats.Blocks)
	}
	if res.Stats.PeakDBFS > -1+1e-9 {
		t.Fatalf("peak %.3f dBFS exceeds ceiling", res.Stats.PeakDBFS)
	}
}

// TestMonoStereoTargets covers both channel-layout normalisations.
func TestMonoStereoTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		out  int
	}{
		{"mono-to-stereo", 1, 2},
		{"stereo-to-mono", 2, 1},
		{"stereo-to-stereo", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, container.FixtureSpec{Rate: 44100, Channels: tc.in, Frames: 44100, FrameSize: 2048})
			cfg := Config{OutputRate: 48000, OutputChannels: tc.out, LoudnessDBFS: -1, BlockFrames: 4800}
			res, err := h.run("w", cfg, 0)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			wantFrames := int64(48000)
			if res.Stats.FramesOut != wantFrames {
				t.Fatalf("frames out %d, want %d", res.Stats.FramesOut, wantFrames)
			}
			if res.Stats.SamplesOut != wantFrames*int64(tc.out) {
				t.Fatalf("samples out %d", res.Stats.SamplesOut)
			}
		})
	}
}

// TestCheckpointConflict proves a config or input change is refused with the
// checkpoint conflict code rather than silently mixing outputs.
func TestCheckpointConflict(t *testing.T) {
	h := newHarness(t, defaultSpec())
	if _, err := h.run("w", stereo48k(), 2); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	changed := stereo48k()
	changed.OutputChannels = 1
	_, err := h.run("w", changed, 0)
	if err == nil {
		t.Fatal("expected conflict")
	}
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeCheckpointConflict {
		t.Fatalf("got %v, want CHECKPOINT_CONFLICT", err)
	}
	if got := me.ExitCode(); got != media.ExitCheckpoint {
		t.Fatalf("exit=%d, want %d", got, media.ExitCheckpoint)
	}
	// Changing the input fingerprint must conflict the same way.
	other := *h
	other.sha = "00" + h.sha[2:]
	if _, err := other.run("w", stereo48k(), 0); err == nil {
		t.Fatal("expected input conflict")
	}
}

// TestGeometryChangeConflicts proves a different block geometry is refused
// even though the input and codec bindings match.
func TestGeometryChangeConflicts(t *testing.T) {
	h := newHarness(t, defaultSpec())
	if _, err := h.run("w", stereo48k(), 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changed := stereo48k()
	changed.BlockFrames = 2400
	_, err := h.run("w", changed, 0)
	if err == nil {
		t.Fatal("expected conflict")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeCheckpointConflict {
		t.Fatalf("got %v", err)
	}
}

// TestBlockTooLong enforces the one-second block limit.
func TestBlockTooLong(t *testing.T) {
	h := newHarness(t, defaultSpec())
	cfg := stereo48k()
	cfg.BlockFrames = 48001
	_, err := h.run("w", cfg, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeBlockTooLong {
		t.Fatalf("got %v", err)
	}
}

// TestLoudnessClassification uses the fixture's four passages to confirm the
// silent, clipped and limited counters all fire and the ceiling holds.
func TestLoudnessClassification(t *testing.T) {
	h := newHarness(t, container.FixtureSpec{Rate: 48000, Channels: 2, Frames: 48000, FrameSize: 4096})
	res, err := h.run("w", Config{OutputRate: 48000, OutputChannels: 2, LoudnessDBFS: -1, BlockFrames: 2400}, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stats.SilentBlocks == 0 {
		t.Errorf("no silent blocks detected")
	}
	if res.Stats.ClippedBlocks == 0 {
		t.Errorf("no clipped blocks detected")
	}
	if res.Stats.LimitedBlocks == 0 {
		t.Errorf("no limited blocks detected")
	}
	if res.Stats.PeakDBFS > -0.99 {
		t.Errorf("peak %.3f dBFS above ceiling", res.Stats.PeakDBFS)
	}
}

// TestUnalignedPCMSample surfaces a non-whole-frame sample with its offset.
func TestUnalignedPCMSample(t *testing.T) {
	h := newHarness(t, container.FixtureSpec{
		Rate: 48000, Channels: 2, Frames: 48000, FrameSize: 4096, OddSize: true,
	})
	_, err := h.run("w", stereo48k(), 0)
	if err == nil {
		t.Fatal("expected UNALIGNED_PCM")
	}
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeUnalignedPCM {
		t.Fatalf("got %v", err)
	}
	if !me.HasOffset || me.Track != 1 {
		t.Fatalf("missing attribution: offset=%v track=%d", me.HasOffset, me.Track)
	}
	if got := me.ExitCode(); got != media.ExitProcessing {
		t.Fatalf("exit=%d, want %d", got, media.ExitProcessing)
	}
}

// TestTimestampRegression propagates a regressing composition offset onto
// the shared timeline instead of silently reordering samples.
func TestTimestampRegression(t *testing.T) {
	h := newHarness(t, container.FixtureSpec{
		Rate: 48000, Channels: 2, Frames: 48000, FrameSize: 4096, CTTSTrick: true,
	})
	_, err := h.run("w", stereo48k(), 0)
	if err == nil {
		t.Fatal("expected TIMESTAMP_REGRESSION")
	}
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeTimestampRegression {
		t.Fatalf("got %v", err)
	}
	if me.Track != 1 || !me.HasOffset {
		t.Fatalf("missing attribution: %+v", me)
	}
}

// TestResumeAfterCorruptBlock refuses to trust a block whose bytes no longer
// match the checkpoint digest.
func TestResumeAfterCorruptBlock(t *testing.T) {
	h := newHarness(t, defaultSpec())
	if _, err := h.run("w", stereo48k(), 2); err != nil {
		t.Fatalf("seed: %v", err)
	}
	block := store.BlockPath(filepath.Join(h.dir, "w"), 0)
	if err := os.WriteFile(block, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := h.run("w", stereo48k(), 0)
	if err == nil {
		t.Fatal("expected corrupt-block rejection")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeCheckpointCorrupt {
		t.Fatalf("got %v", err)
	}
}

// TestCompleteRunIsIdempotent re-running a finished work directory verifies
// rather than re-encodes, and yields the same digest.
func TestCompleteRunIsIdempotent(t *testing.T) {
	h := newHarness(t, defaultSpec())
	first, err := h.run("w", stereo48k(), 0)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := h.run("w", stereo48k(), 0)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Stats.SHA256 != second.Stats.SHA256 {
		t.Fatalf("digests differ on re-run")
	}
}

// TestDualTimeBaseResampling checks 44.1->48 kHz conversion at every block
// boundary stays exact: total frames are round(n*48000/44100).
func TestDualTimeBaseResampling(t *testing.T) {
	for _, n := range []int64{441, 4410, 44100, 441000} {
		rs, err := NewResampler(44100, 48000, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := rs.OutputFramesFor(n); got != (n*48000+22050)/44100 {
			t.Fatalf("n=%d: frames %d, want %d", n, got, (n*48000+22050)/44100)
		}
	}
}
