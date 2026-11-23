package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"medpipe/internal/dag"
	"medpipe/internal/model"
	"medpipe/internal/sequence"
	"medpipe/internal/synth"
)

func writeCapture(t *testing.T, dir string, cap *synth.Capture) string {
	t.Helper()
	path := filepath.Join(dir, "capture.medpseq")
	if _, _, err := sequence.WriteFile(path, cap.Tracks, cap.Frames); err != nil {
		t.Fatal(err)
	}
	return path
}

func baseConfig(dir string) Config {
	return Config{
		InputPath: "capture.medpseq", WorkDir: dir, StatePath: filepath.Join(dir, "state", "nodes.json"),
		WindowNS: 2_000_000_000, SilenceWindowNS: 20_000_000, SilenceThreshold: 0.05, MaxAttempts: 3,
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 42, Tracks: 3, Frames: 240, DriftPPM: 180, WrapAt: 65536, VFR: true})
	if err != nil {
		t.Fatal(err)
	}
	writeCapture(t, dir, cap)
	cfg := baseConfig(dir)
	cfg.SidecarPath = "manifest.mp4"
	out, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Segments) < 1 {
		t.Fatal("no segments")
	}
	if len(out.Fingerprints.Segments) != len(out.Segments) {
		t.Fatalf("fingerprint count %d != segment count %d", len(out.Fingerprints.Segments), len(out.Segments))
	}
	if out.Sidecar == nil || len(out.Sidecar.Tracks) != 3 {
		t.Fatalf("sidecar summary = %+v", out.Sidecar)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.mp4")); err != nil {
		t.Fatalf("sidecar file missing: %v", err)
	}
	rate, err := RoundTripRate(out)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1 {
		t.Fatalf("round trip rate = %v", rate)
	}
	if got := KeyframeHitRate(out); got != 1 {
		t.Fatalf("keyframe hit rate = %v", got)
	}
	// Every segment's boundary must agree across tracks: slices share the
	// segment's half-open interval exactly.
	for _, seg := range out.Segments {
		for _, sl := range seg.Slices {
			if sl.StartNS != seg.StartNS || sl.EndNS != seg.EndNS {
				t.Fatalf("segment %d track %s bounds diverge", seg.Index, sl.TrackID)
			}
		}
	}
}

func TestPipelineReusesCompletedNodes(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 7, Tracks: 2, Frames: 120, VFR: true})
	if err != nil {
		t.Fatal(err)
	}
	writeCapture(t, dir, cap)
	cfg := baseConfig(dir)
	if _, err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	out, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ReusedNodes != 6 {
		t.Fatalf("second run reused %d nodes, want 6", out.Run.ReusedNodes)
	}
	if out.Run.ExecutedNodes != 0 {
		t.Fatalf("second run executed %d nodes, want 0", out.Run.ExecutedNodes)
	}
}

func TestPipelineRejectsChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 3, Tracks: 1, Frames: 40})
	if err != nil {
		t.Fatal(err)
	}
	path := writeCapture(t, dir, cap)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := Run(baseConfig(dir))
	if err == nil {
		t.Fatalf("corrupt input accepted: %+v", out)
	}
	if !model.IsKind(err, model.KindCorrupt) && !model.IsKind(err, model.KindInput) {
		t.Fatalf("error kind = %v", err)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit code = %d, want 2", model.ExitCode(err))
	}
}

func TestPipelineBlocksAndRecoversFromDamagedFraming(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 11, Tracks: 3, Frames: 120, VFR: true, WrapAt: 65536})
	if err != nil {
		t.Fatal(err)
	}
	path := writeCapture(t, dir, cap)
	cfg := baseConfig(dir)
	if _, err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Damage the codec tag of a video frame while keeping its record checksum
	// self-consistent: only content validation can catch this.
	damaged := clone(cap.Frames)
	video := firstTrackOfKind(cap, model.KindVideo)
	fr := &damaged[video][1]
	fr.Payload = append([]byte(nil), fr.Payload...)
	fr.Payload[0] = 'X'
	fr.Checksum = fr.IntegrityChecksum()
	if _, _, err := sequence.WriteFile(path, cap.Tracks, damaged); err != nil {
		t.Fatal(err)
	}
	_, err = Run(cfg)
	failure, ok := AsFailure(err)
	if !ok {
		t.Fatalf("expected a graph failure carrying the partial run, got %v", err)
	}
	if failure.Run.FirstBlocked == nil {
		t.Fatalf("no blocked dependency reported")
	}
	if failure.Run.FirstBlocked.Dep != NodeSegment {
		t.Fatalf("blocked by %s, want %s", failure.Run.FirstBlocked.Dep, NodeSegment)
	}
	if failure.Run.Results[NodeFingerprint].State != dag.StateBlocked {
		t.Fatalf("fingerprint state = %s", failure.Run.Results[NodeFingerprint].State)
	}
	if failure.Run.Results[NodeRead].State != dag.StateDone {
		t.Fatalf("read state = %s, want done (upstream must stay completed)", failure.Run.Results[NodeRead].State)
	}

	// Repair the input and resume: everything is reused, nothing duplicated.
	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ReusedNodes != 6 {
		t.Fatalf("recovery reused %d nodes, want 6", out.Run.ReusedNodes)
	}
	// The store keeps one record per idempotency key: the damaged pass added
	// records only for the nodes that completed under the damaged digest, and
	// resuming must not append anything further.
	store, err := dag.OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	afterRepair := store.Len()
	if _, err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	store2, err := dag.OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if store2.Len() != afterRepair {
		t.Fatalf("re-running an unchanged graph appended records: %d -> %d", afterRepair, store2.Len())
	}
	keys := store2.Keys()
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate record key %s", k)
		}
		seen[k] = true
	}
}

func TestPipelineRepairsCorruptionOnce(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 5, Tracks: 2, Frames: 60})
	if err != nil {
		t.Fatal(err)
	}
	path := writeCapture(t, dir, cap)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Derive a bad variant by flipping a payload byte.
	bad := append([]byte(nil), good...)
	bad[len(bad)/2] ^= 0xff
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(dir)
	repairedCalls := 0
	cfg.OnCorrupt = func(me *model.Error) error {
		repairedCalls++
		if me.TrackID == "" || me.Offset == 0 {
			t.Errorf("repair hook lost track/offset: %+v", me)
		}
		return os.WriteFile(path, good, 0o644)
	}
	out, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if repairedCalls != 1 {
		t.Fatalf("repair hook called %d times, want 1", repairedCalls)
	}
	if len(out.Segments) == 0 {
		t.Fatal("repaired run produced no segments")
	}
	// The repaired run must key the read node on the repaired bytes, so a
	// subsequent run is a pure cache hit rather than another re-execution.
	again, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again.Run.ReusedNodes != 6 || again.Run.ExecutedNodes != 0 {
		t.Fatalf("post-repair rerun executed %d nodes, reused %d", again.Run.ExecutedNodes, again.Run.ReusedNodes)
	}
}

func TestPipelineRejectsBadConfig(t *testing.T) {
	if _, err := Run(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	dir := t.TempDir()
	cfg := baseConfig(dir)
	cfg.InputPath = "../escape.medpseq"
	if _, err := Run(cfg); err == nil {
		t.Fatal("path escape accepted")
	}
	cfg = baseConfig(dir)
	cfg.StatePath = ""
	if _, err := Run(cfg); err == nil {
		t.Fatal("missing state path accepted")
	}
}

func TestReportLinesAreStable(t *testing.T) {
	dir := t.TempDir()
	cap, err := synth.Generate(synth.Options{Seed: 42, Tracks: 3, Frames: 120, VFR: true, WrapAt: 65536})
	if err != nil {
		t.Fatal(err)
	}
	writeCapture(t, dir, cap)
	out, err := Run(baseConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := BuildReport(out, 0, &dag.BlockedDep{Node: NodeFingerprint, Dep: NodeSegment}, 1)
	if err != nil {
		t.Fatal(err)
	}
	lines := rep.Lines()
	want := []string{"roundtrip_rate=", "keyframe_hit_rate=", "segments=", "segment_sha256_count=",
		"corrupt_rejected=", "recovery_reused_nodes=", "blocked_dependency="}
	for i, prefix := range want {
		if len(lines[i]) < len(prefix) || lines[i][:len(prefix)] != prefix {
			t.Fatalf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	if rep.BlockedDependency != NodeFingerprint {
		t.Fatalf("blocked dependency = %q", rep.BlockedDependency)
	}

	// Determinism: the same graph run twice yields identical hashes.
	out2, err := Run(baseConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range out.Fingerprints.Segments {
		if out.Fingerprints.Segments[i].SHA256 != out2.Fingerprints.Segments[i].SHA256 {
			t.Fatalf("segment %d hash not deterministic", i)
		}
	}
}

func clone(in map[string][]model.Frame) map[string][]model.Frame {
	out := make(map[string][]model.Frame, len(in))
	for k, v := range in {
		cp := make([]model.Frame, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func firstTrackOfKind(c *synth.Capture, kind model.Kind) string {
	for _, tr := range c.Tracks {
		if tr.Kind == kind {
			return tr.ID
		}
	}
	return ""
}
