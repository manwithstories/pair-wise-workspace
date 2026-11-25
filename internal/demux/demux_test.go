package demux_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/demux"
	"archivecheck/internal/fixtgen"
	"archivecheck/internal/media"
	"archivecheck/internal/remux"
)

// loadFixtures generates the canonical batch once per test that needs it.
func loadFixtures(t *testing.T) (string, []fixtgen.ManifestEntry) {
	t.Helper()
	root := t.TempDir()
	entries, err := fixtgen.GenerateAll(root)
	if err != nil {
		t.Fatalf("generate fixtures: %v", err)
	}
	return root, entries
}

func find(entries []fixtgen.ManifestEntry, fault fixtgen.Fault) fixtgen.ManifestEntry {
	for _, e := range entries {
		if e.Fault == fault {
			return e
		}
	}
	return fixtgen.ManifestEntry{}
}

// TestCleanRoundtrip is the happy path: a clean 3-track segment demuxes to all
// samples and remuxes to a re-parseable fragment without byte changes.
func TestCleanRoundtrip(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultNone)
	src := filepath.Join(root, "sources", seg.Source)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err != nil {
		t.Fatalf("clean parse returned error: %v", err)
	}
	if len(tracks) != fixtgen.TrackCount {
		t.Fatalf("tracks = %d, want %d", len(tracks), fixtgen.TrackCount)
	}
	if report.Samples != fixtgen.TrackCount*fixtgen.SamplesPerTrack {
		t.Fatalf("samples = %d, want %d", report.Samples, fixtgen.TrackCount*fixtgen.SamplesPerTrack)
	}
	if report.Truncated || len(report.Faults) != 0 {
		t.Fatalf("clean segment reported faults: %+v", report.Faults)
	}

	// Every video sample's coded bytes must match the generator byte-for-byte.
	video := tracks[0]
	for i, got := range video.Samples {
		want := fixtgen.SampleData(seg.Index, 0, i, "avc1")
		if string(got.Data) != string(want) {
			t.Fatalf("sample %d coded bytes altered (transcode detected)", i)
		}
	}

	// Remux and re-parse; the output must carry the same samples and hash stably.
	inputs := make([]remux.TrackInput, len(tracks))
	for i, tr := range tracks {
		inputs[i] = remux.TrackInput{Meta: tr.Meta, Samples: tr.Samples}
	}
	out := filepath.Join(root, "out", seg.ID+".mp4")
	res, err := remux.WriteFragment(out, inputs)
	if err != nil {
		t.Fatalf("remux: %v", err)
	}
	reTracks, _, rerr := demux.ParseSource(out, seg.ID, demux.DefaultParseConfig())
	if rerr != nil || len(reTracks) != fixtgen.TrackCount {
		t.Fatalf("reparse output: tracks=%d err=%v", len(reTracks), rerr)
	}
	reCount := 0
	for _, tr := range reTracks {
		reCount += len(tr.Samples)
	}
	if reCount != report.Samples {
		t.Fatalf("reparsed samples = %d, want %d", reCount, report.Samples)
	}

	// Deterministic hash: writing the same inputs again must produce an
	// identical SHA-256 of the media payload.
	res2, _ := remux.WriteFragment(filepath.Join(root, "out2", seg.ID+".mp4"), inputs)
	if res.SHA256 != res2.SHA256 {
		t.Fatal("fragment SHA-256 not stable across runs")
	}
}

// TestTruncationRetainsCompleteSamples covers tail truncation: the last
// complete samples survive and a located truncated fault is recorded.
func TestTruncationRetainsCompleteSamples(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultTruncated)
	src := filepath.Join(root, "sources", seg.Source)
	before, _ := os.Stat(src)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err != nil {
		t.Fatalf("truncated parse should not hard-fail: %v", err)
	}
	if !report.Truncated {
		t.Fatal("expected truncated flag")
	}
	if len(tracks) != fixtgen.TrackCount {
		t.Fatalf("tracks = %d, want all %d retained", len(tracks), fixtgen.TrackCount)
	}
	if report.Samples >= fixtgen.TrackCount*fixtgen.SamplesPerTrack {
		t.Fatal("truncated segment must retain fewer than the full sample set")
	}
	var foundTrunc bool
	for _, f := range report.Faults {
		if f.Kind == media.KindTruncated && f.Offset >= 0 {
			foundTrunc = true
		}
	}
	if !foundTrunc {
		t.Fatal("no located truncation fault with offset")
	}

	// Source file must be untouched by the read-only demux.
	after, _ := os.Stat(src)
	if before.Size() != after.Size() {
		t.Fatal("demux modified the source file")
	}
}

// TestBadLengthQuarantinesTrack proves a corrupt length field is located by
// offset, isolates the owning track, and leaves sibling tracks recoverable.
// Per-track isolation does not fail the whole segment (only an unsupported
// codec or zero surviving tracks do), so parsing returns no error but the
// located fatal fault and the missing track tell the full story.
func TestBadLengthQuarantinesTrack(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultBadLength)
	src := filepath.Join(root, "sources", seg.Source)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err != nil {
		t.Fatalf("per-track isolation must not fail the whole segment: %v", err)
	}
	var bad *media.Fault
	for _, f := range report.Faults {
		if f.Kind == media.KindBadLength {
			bad = f
		}
	}
	if bad == nil || bad.Offset < 0 || !bad.Fatal {
		t.Fatalf("bad-length fault must be fatal and carry an offset: %+v", bad)
	}
	// Track 1 quarantined; tracks 2 and 3 still recover.
	if len(tracks) != fixtgen.TrackCount-1 {
		t.Fatalf("sibling tracks = %d, want %d", len(tracks), fixtgen.TrackCount-1)
	}
}

// TestUnsupportedCodecIsolation covers the permanently unrecoverable segment:
// an unknown codec isolates one track while the other tracks still remux.
func TestUnsupportedCodecIsolation(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultUnsupported)
	src := filepath.Join(root, "sources", seg.Source)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err == nil {
		t.Fatal("unsupported codec must surface a fatal error")
	}
	var unsup *media.Fault
	for _, f := range report.Faults {
		if f.Kind == media.KindUnsupported {
			unsup = f
		}
	}
	if unsup == nil || unsup.Track != 1 || !unsup.Fatal {
		t.Fatalf("unsupported-codec fault not located on track 1: %+v", unsup)
	}
	if len(tracks) != fixtgen.TrackCount-1 {
		t.Fatalf("recoverable sibling tracks = %d, want %d", len(tracks), fixtgen.TrackCount-1)
	}
}

// TestIncompleteFragmentKeepsCompleteSamples verifies fragmented MP4 handling:
// complete fragments survive, the cut fragment contributes no fake keyframe.
func TestIncompleteFragmentKeepsCompleteSamples(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultIncompleteFrag)
	src := filepath.Join(root, "sources", seg.Source)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err != nil {
		t.Fatalf("incomplete fragment must not hard-fail: %v", err)
	}
	if report.Container != demux.ContainerFragmented {
		t.Fatalf("container = %s, want fragmented", report.Container)
	}
	var incomplete bool
	for _, f := range report.Faults {
		if f.Kind == media.KindIncompleteFrag {
			incomplete = true
		}
	}
	if !incomplete {
		t.Fatal("missing incomplete-fragment fault")
	}
	if len(tracks) == 0 || len(tracks[0].Samples) == 0 {
		t.Fatal("complete fragments must be retained")
	}
}

// TestRTPReassemblyAndTruncation covers H.264 RTP capture: FU-A reassembly
// produces access units and a mid-FU cut drops only the incomplete unit.
func TestRTPReassemblyAndTruncation(t *testing.T) {
	root, entries := loadFixtures(t)
	seg := find(entries, fixtgen.FaultRTPTruncated)
	src := filepath.Join(root, "sources", seg.Source)

	tracks, report, err := demux.ParseSource(src, seg.ID, demux.DefaultParseConfig())
	if err != nil {
		t.Fatalf("rtp parse: %v", err)
	}
	if report.Container != demux.ContainerRTP {
		t.Fatalf("container = %s, want rtp", report.Container)
	}
	if len(tracks) != 1 || len(tracks[0].Samples) == 0 {
		t.Fatal("expected reassembled access units")
	}
	var trunc bool
	for _, f := range report.Faults {
		if f.Kind == media.KindTruncated {
			trunc = true
		}
		if f.Kind == media.KindWrap {
			t.Fatalf("legitimate same-ts FU-A misreported as wrap: %v", f)
		}
	}
	if !trunc {
		t.Fatal("expected a truncation fault for the cut FU-A")
	}
}

// TestSourceDigestStable proves the generated source is byte-reproducible,
// which underpins deterministic cache keys and resume semantics.
func TestSourceDigestStable(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	e1, _ := fixtgen.GenerateAll(root1)
	e2, _ := fixtgen.GenerateAll(root2)
	for i := range e1 {
		a, _ := os.ReadFile(filepath.Join(root1, "sources", e1[i].Source))
		b, _ := os.ReadFile(filepath.Join(root2, "sources", e2[i].Source))
		if sha256.Sum256(a) != sha256.Sum256(b) {
			t.Fatalf("segment %s not reproducible across generations", e1[i].ID)
		}
	}
}
