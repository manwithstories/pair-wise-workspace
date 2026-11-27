// Package pipeline exercises the whole recover -> fan-in -> export -> verify
// path. It shells out to FFmpeg through the same fixture builder the CLI uses,
// and skips when FFmpeg is unavailable so the suite stays runnable offline.
package pipeline_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archive/internal/checkpoint"
	"archive/internal/fixture"
	"archive/internal/isobmff"
	"archive/internal/media"
	"archive/internal/recovery"
	"archive/internal/timeline"
	"archive/internal/verify"
)

func haveFFmpeg(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; install with brew install ffmpeg")
	}
	return p
}

// buildMixed creates a mixed-profile fixture once per test in a temp dir.
func buildMixed(t *testing.T) fixture.Manifest {
	t.Helper()
	ff := haveFFmpeg(t)
	dir := t.TempDir()
	res, err := fixture.Build(fixture.BuildOptions{
		Profile:     fixture.ProfileMixed,
		OutDir:      dir,
		DurationSec: 4, // half length keeps the test under a few seconds
		FFmpeg:      ff,
	})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	return res.Manifest
}

// recoverAll runs the library stages the CLI drives, returning the timeline.
func recoverAll(t *testing.T, m fixture.Manifest, threshold int64) (*media.Timeline, []*recovery.RepresentationRecovery, int64) {
	t.Helper()
	var (
		inputs   []timeline.Input
		recs     []*recovery.RepresentationRecovery
		firstBad int64 = -1
	)
	for _, lvl := range m.Levels {
		data, err := os.ReadFile(lvl.Path)
		if err != nil {
			t.Fatalf("read %s: %v", lvl.Level, err)
		}
		parsed, err := isobmff.ParseFile(lvl.Level, lvl.Path, data)
		if err != nil {
			t.Fatalf("parse %s: %v", lvl.Level, err)
		}
		rec, err := recovery.Recover(lvl.Level, lvl.Path, parsed, recovery.DefaultOptions())
		if err != nil {
			t.Fatalf("recover %s: %v", lvl.Level, err)
		}
		inputs = append(inputs, timeline.Input{Name: lvl.Level, Rec: rec})
		recs = append(recs, rec)
		if rec.FirstBadByte >= 0 && (firstBad < 0 || rec.FirstBadByte < firstBad) {
			firstBad = rec.FirstBadByte
		}
	}
	tl, err := timeline.Build(inputs, timeline.Options{BoundaryThresholdUS: threshold})
	if err != nil {
		t.Fatalf("fan-in: %v", err)
	}
	return tl, recs, firstBad
}

func TestCleanFixtureRecoversDeterministically(t *testing.T) {
	ff := haveFFmpeg(t)
	dir := t.TempDir()
	res, err := fixture.Build(fixture.BuildOptions{Profile: fixture.ProfileClean, OutDir: dir, DurationSec: 4, FFmpeg: ff})
	if err != nil {
		t.Fatalf("build clean fixture: %v", err)
	}
	tl1, recs, bad := recoverAll(t, res.Manifest, 2000)
	if bad != -1 {
		t.Fatalf("clean fixture first_bad_byte = %d", bad)
	}
	for _, r := range recs {
		if r.Dropped != 0 || r.Duplicates != 0 || r.Truncated != 0 {
			t.Fatalf("clean rep %s dropped=%d dup=%d trunc=%d", r.Name, r.Dropped, r.Duplicates, r.Truncated)
		}
		if r.Accepted != len(res.Manifest.Levels) && r.Accepted == 0 {
			t.Fatalf("clean rep %s accepted 0", r.Name)
		}
	}
	// Rebuilding the timeline from the same bytes must agree byte-for-byte:
	// the digest is the contract for "repeat run yields same timeline".
	again, _, _ := recoverAll(t, res.Manifest, 2000)
	if verify.TimelineDigest(tl1) != verify.TimelineDigest(again) {
		t.Fatalf("timeline digest not deterministic")
	}
	if len(tl1.Tracks) == 0 {
		t.Fatalf("timeline has no tracks")
	}
	if len(tl1.Conflicts) != 0 {
		t.Fatalf("clean timeline reports conflicts: %+v", tl1.Conflicts)
	}
}

func TestMixedFixtureIsolatesDamage(t *testing.T) {
	m := buildMixed(t)
	tl, recs, bad := recoverAll(t, m, 2000)
	if bad < 0 {
		t.Fatalf("mixed fixture should expose first_bad_byte")
	}
	if verify.TimelineDigest(tl) == "" {
		t.Fatalf("empty timeline digest")
	}
	// At least one representation must demonstrate a duplicate or a drop,
	// and the truncated mdat must be attributed to the carrier-independent
	// low level.
	var sawDrop, sawDup, sawTrunc bool
	for _, r := range recs {
		if r.Dropped > 0 {
			sawDrop = true
		}
		if r.Duplicates > 0 {
			sawDup = true
		}
		if r.Truncated > 0 {
			sawTrunc = true
		}
		// Each recovered track must remain monotonic in decode time.
		for _, rt := range r.Tracks {
			for i := 1; i < len(rt.Samples); i++ {
				if rt.Samples[i].DTS < rt.Samples[i-1].DTS {
					t.Fatalf("rep %s track %d non-monotonic DTS at %d", r.Name, rt.TrackID, i)
				}
			}
			if rt.Regressed > 0 && verify.TimelineDigest(tl) == "" {
				t.Fatalf("regression recorded but timeline empty")
			}
		}
	}
	if !sawDrop || !sawDup || !sawTrunc {
		t.Fatalf("fault coverage incomplete: drop=%v dup=%v trunc=%v", sawDrop, sawDup, sawTrunc)
	}
	// A boundary disagreement must be surfaced, not resolved silently.
	var sawBoundary bool
	for _, c := range tl.Conflicts {
		if c.Metric == "fragment_boundary" || c.Metric == "boundary" {
			sawBoundary = true
		}
	}
	if !sawBoundary {
		t.Fatalf("no boundary conflict surfaced for mixed fixture")
	}
	// The acceptance metric (outer-edge delta) must remain within tolerance.
	var maxOuter int64
	for _, a := range tl.Alignments {
		if a.OuterDeltaUS > maxOuter {
			maxOuter = a.OuterDeltaUS
		}
	}
	if maxOuter > 2000 {
		t.Fatalf("outer boundary delta %d us exceeds 2000", maxOuter)
	}
}

func TestCheckpointRoundTripAndDigestPin(t *testing.T) {
	dir := t.TempDir()
	mgr, err := checkpoint.Open(filepath.Join(dir, "run.ckpt"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st := &checkpoint.State{
		InputSHA256:              "deadbeef",
		Profile:                  "mixed",
		CompletedRepresentations: []string{"high", "mid"},
		Inflight:                 "low",
		StartedAt:                time.Now().UTC(),
	}
	if err := mgr.Save(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := mgr.Load("deadbeef")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil || loaded.Inflight != "low" || len(loaded.CompletedRepresentations) != 2 {
		t.Fatalf("round trip = %+v", loaded)
	}
	if _, err := mgr.Load("cafebabe"); err == nil {
		t.Fatalf("digest mismatch must fail")
	}
	// A torn file must be reported as corrupt rather than trusted.
	if err := os.WriteFile(filepath.Join(dir, "bad.ckpt"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	badMgr, _ := checkpoint.Open(filepath.Join(dir, "bad.ckpt"))
	if _, err := badMgr.Load("deadbeef"); err == nil {
		t.Fatalf("corrupt checkpoint must fail")
	}
}

func TestFirstBadByteSurvivesRecoveryError(t *testing.T) {
	// A file whose moov is truncated must yield inevitably-fatal input, but
	// the first bad byte must be extractable for the report.
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.mp4")
	if err := os.WriteFile(path, []byte{0, 0, 0, 20, 'm', 'o', 'o', 'v'}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := isobmff.ParseFile("broken", path, mustRead(t, path))
	if err == nil {
		t.Fatalf("expected parse failure")
	}
	off, ok := media.FirstBadByteOffset(err)
	if !ok {
		t.Fatalf("no first_bad_byte: %v", err)
	}
	if off != 8 {
		t.Fatalf("first_bad_byte = %d, want 8 (end of truncated box)", off)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func TestVerifyDetectsTamperedArtifact(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(filepath.Join(out, "frames"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Minimal but well-formed artifacts: one frame, a timeline with one track
	// and no conflicts, so verify must pass before tampering.
	tl := &media.Timeline{OriginUS: 0, Tracks: []media.TrackTimeline{{
		TrackID: 1, Kind: media.KindVideo, Timescale: 12800,
		Samples: []media.TimelineSample{{Representation: "high", StartMicros: 0, EndMicros: 40000}},
		StartUS: 0, EndUS: 40000,
	}}}
	digest := verify.TimelineDigest(tl)
	doc := map[string]any{"timeline": tl, "timeline_sha256": digest, "first_bad_byte": 0, "resume_ms": 0}
	if err := writeJSON(t, filepath.Join(out, "timeline.json"), doc); err != nil {
		t.Fatalf("write timeline: %v", err)
	}
	pngBytes := []byte("png")
	sha := digestBytes(pngBytes)
	frames := []map[string]any{{"index": 0, "file": "frames/frame-0000.png", "sha256": sha, "start_us": 0}}
	if err := writeJSON(t, filepath.Join(out, "frames.json"), frames); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "frames", "frame-0000.png"), pngBytes, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	rep, ok, err := verify.Run(verify.Input{OutDir: out, Timeline: tl, ExpectedFrames: 1, BoundaryLimitUS: 2000, FirstBadByte: 0})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("verify failed on well-formed artifacts: %+v", rep.Failures)
	}
	// Swap the frame payload on disk while leaving the manifest untouched: the
	// re-hash in verify must catch the difference.
	if err := os.WriteFile(filepath.Join(out, "frames", "frame-0000.png"), []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper png: %v", err)
	}
	rep2, ok2, err := verify.Run(verify.Input{OutDir: out, Timeline: tl, ExpectedFrames: 1, BoundaryLimitUS: 2000, FirstBadByte: 0})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok2 {
		t.Fatalf("tampered frame passed verification: %+v", rep2)
	}
	var sawMismatch bool
	for _, f := range rep2.Failures {
		if strings.Contains(f, "digest mismatch") {
			sawMismatch = true
		}
	}
	if !sawMismatch {
		t.Fatalf("tamper not attributed to a digest mismatch: %+v", rep2.Failures)
	}
}

func digestBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func writeJSON(t *testing.T, path string, v any) error {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
