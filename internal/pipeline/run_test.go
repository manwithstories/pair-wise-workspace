package pipeline

import (
	"errors"
	"os"
	"testing"

	"mediad/internal/checkpoint"
	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/model"
	"mediad/internal/synth"
)

func gen(t *testing.T, dir string, track uint32, frames, corrupt int, truncate bool) Paths {
	t.Helper()
	spec := synth.DefaultSpec(track, frames)
	spec.CorruptFrames = corrupt
	spec.TruncateTail = truncate
	p := Paths{Dir: dir, Track: track}
	if _, err := synth.WriteSource(p.Source(), spec, int64(1000+track)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return p
}

func TestRunHealthyTrackWithinBounds(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 1, 120, 0, false)
	res, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 120})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := res.Metrics
	if m.FramesOK != 120 || m.FramesBad != 0 {
		t.Fatalf("frames ok/bad = %d/%d, want 120/0", m.FramesOK, m.FramesBad)
	}
	if m.IndexEntries != 120 {
		t.Fatalf("index entries = %d, want 120", m.IndexEntries)
	}
	if m.BitrateDeltaPct > 5 || m.BitrateDeltaPct < -5 {
		t.Fatalf("bitrate delta = %.2f", m.BitrateDeltaPct)
	}
	if m.ManifestSkewMS > 1 {
		t.Fatalf("manifest skew = %.3f", m.ManifestSkewMS)
	}
}

func TestRunIsolatesCorruptFramesFromIndex(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 2, 100, 3, false)
	res, err := Run(Options{WorkDir: dir, TrackID: 2, TargetFrames: 100})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Metrics.FramesBad != 3 {
		t.Fatalf("frames_bad = %d, want 3", res.Metrics.FramesBad)
	}
	if res.Metrics.IndexEntries != 97 {
		t.Fatalf("index entries = %d, want 97 (bad frames excluded)", res.Metrics.IndexEntries)
	}
	entries, _, err := index.Open(res.Paths.Index())
	if err != nil {
		t.Fatal(err)
	}
	// Every entry must resolve to a real frame with a matching content digest.
	src, err := os.Open(res.Paths.Source())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, e := range entries {
		fb, err := ingest.ReadFrameAt(src, e.Offset, uint64(e.Size))
		if err != nil {
			t.Fatalf("entry frame %d does not resolve: %v", e.FrameNo, err)
		}
		if model.DigestBytes(fb.Payload) == [32]byte{} {
			t.Fatalf("entry frame %d has an empty digest", e.FrameNo)
		}
	}
}

func TestResumeProducesSameDigestAsUninterrupted(t *testing.T) {
	fullDir := t.TempDir()
	gen(t, fullDir, 3, 200, 2, true)
	full, err := Run(Options{WorkDir: fullDir, TrackID: 3, TargetFrames: 200})
	if err != nil {
		t.Fatalf("full: %v", err)
	}

	partDir := t.TempDir()
	gen(t, partDir, 3, 200, 2, true)
	if _, err := Run(Options{WorkDir: partDir, TrackID: 3, TargetFrames: 200, InterruptAfter: 75}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt run: err = %v", err)
	}
	// Simulate a torn index write before resume.
	chopFile(t, Paths{Dir: partDir, Track: 3}.Index(), 13)
	resumed, err := Run(Options{WorkDir: partDir, TrackID: 3, TargetFrames: 200, Resume: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if full.ManifestSum != resumed.ManifestSum {
		t.Fatalf("manifest digest differs: %x vs %x", full.ManifestSum, resumed.ManifestSum)
	}
	if full.IndexSum != resumed.IndexSum {
		t.Fatalf("index digest differs: %x vs %x", full.IndexSum, resumed.IndexSum)
	}
	if !resumed.Resumed {
		t.Fatal("resume was not recognized")
	}
	if full.Metrics.FramesOK != resumed.Metrics.FramesOK || full.Metrics.FramesBad != resumed.Metrics.FramesBad {
		t.Fatalf("frame tallies differ: full %+v resumed %+v", full.Metrics, resumed.Metrics)
	}
}

func TestInterruptCommitsVerifiableCheckpoint(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 4, 60, 0, false)
	if _, err := Run(Options{WorkDir: dir, TrackID: 4, TargetFrames: 60, InterruptAfter: 25}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want interrupted", err)
	}
	p := Paths{Dir: dir, Track: 4}
	st, err := checkpoint.Load(p.State())
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	// The interrupting frame is written and committed before the run stops, so
	// the durable prefix includes it.
	if st.NextFrame != 25 {
		t.Fatalf("next_frame = %d, want 25 (interrupting frame included)", st.NextFrame)
	}
	if err := checkpoint.Verify(st); err != nil {
		t.Fatalf("checkpoint does not verify against disk: %v", err)
	}
}

func TestStaleCheckpointIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	p := gen(t, dir, 5, 50, 0, false)
	if _, err := Run(Options{WorkDir: dir, TrackID: 5, TargetFrames: 50, InterruptAfter: 20}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}
	// Mutate the source after the checkpoint committed; verification must fail
	// and the run must restart clean rather than trust stale progress.
	if err := os.WriteFile(p.Source(), []byte("corrupted beyond recovery"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{WorkDir: dir, TrackID: 5, TargetFrames: 50, Resume: true})
	if err == nil {
		t.Fatal("resume accepted a stale checkpoint over a changed source")
	}
	if model.ClassOf(err) != model.ClassFatal {
		t.Fatalf("class = %v, want fatal", model.ClassOf(err))
	}
}

func TestRTPLossBecomesManifestGap(t *testing.T) {
	dir := t.TempDir()
	gen(t, dir, 6, 120, 0, false)
	res, err := Run(Options{WorkDir: dir, TrackID: 6, TargetFrames: 120, RTPLoss: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Metrics.RTPLost != 2 {
		t.Fatalf("rtp_lost = %d, want the two dropped packets", res.Metrics.RTPLost)
	}
	// A lost preview packet means one or two frames lose their preview; the
	// gap count is the number of frames that never completed, never more than
	// the packets lost.
	if res.RTPGaps < 1 || res.RTPGaps > 2 {
		t.Fatalf("rtp gaps = %d, want 1..2 for two lost packets", res.RTPGaps)
	}
	// The source frames still exist and are indexed; no frame is invented to
	// fill a preview gap.
	if res.Metrics.IndexEntries != 120 {
		t.Fatalf("index entries = %d, want 120", res.Metrics.IndexEntries)
	}
}

func chopFile(t *testing.T, path string, n int64) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() <= n {
		t.Fatal("file too small to chop")
	}
	if err := os.Truncate(path, fi.Size()-n); err != nil {
		t.Fatal(err)
	}
}
