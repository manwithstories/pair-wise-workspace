package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"medialine/internal/align"
	"medialine/internal/fault"
	"medialine/internal/index"
	"medialine/internal/model"
)

func mkTrack(id int, timescale uint32, starts []int64, keys map[int]bool) *model.Track {
	tr := &model.Track{ID: id, Timescale: timescale, Codec: "vide"}
	for i, s := range starts {
		tr.Samples = append(tr.Samples, model.Sample{Number: i + 1, PTSUS: s, DTSUS: s, DurationUS: 500, Keyframe: keys[i+1]})
	}
	return tr
}

func gridStarts(span, step int64) []int64 {
	var out []int64
	for t := int64(0); t < span; t += step {
		out = append(out, t)
	}
	return out
}

func allKeys(n int) map[int]bool {
	m := map[int]bool{}
	for i := 1; i <= n; i++ {
		m[i] = true
	}
	return m
}

func setupLadder(t *testing.T, span int64) ([]align.Representation, map[int]*model.Track, map[string]*index.TrackIndex, *align.Aligned) {
	t.Helper()
	tr1 := mkTrack(1, 90000, gridStarts(span, 500_000), allKeys(int(span/500_000)))
	tr2 := mkTrack(2, 90000, gridStarts(span, 250_000), allKeys(int(span/250_000)))
	tracks := map[int]*model.Track{1: tr1, 2: tr2}
	idx1, _ := index.Build(tr1)
	idx2, _ := index.Build(tr2)
	reps := []align.Representation{
		{ID: "v1080", Bitrate: 6_000_000, TrackID: 1, Index: idx1},
		{ID: "v720", Bitrate: 3_000_000, TrackID: 2, Index: idx2},
	}
	al, err := align.Align(reps, span, align.Config{SegmentUS: 2_000_000, SnapBack: true})
	if err != nil {
		t.Fatal(err)
	}
	return reps, tracks, map[string]*index.TrackIndex{"v1080": idx1, "v720": idx2}, al
}

func TestBuilderEntriesAndManifest(t *testing.T) {
	span := int64(6_000_000)
	reps, tracks, idx, al := setupLadder(t, span)
	dir := t.TempDir()
	b, err := NewBuilder(Options{SegmentUS: 2_000_000, SnapBack: true, OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reps {
		if err := b.AddRepresentation(Representation{ID: r.ID, Bitrate: r.Bitrate, TrackID: r.TrackID, Codec: "avc1", Timescale: 90000}); err != nil {
			t.Fatal(err)
		}
	}
	simple, composite, err := b.AlignEntries(al, reps, tracks, idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(composite) != 0 {
		t.Fatalf("unexpected composites: %d", len(composite))
	}
	if len(simple) != 6 { // 3 segments x 2 reps
		t.Fatalf("entries=%d", len(simple))
	}
	for _, e := range simple {
		b.Append(e)
	}
	m := b.Build(span, nil)
	path, err := Write(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	read, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Entries) != 6 {
		t.Fatalf("reread entries=%d", len(read.Entries))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// Deterministic ordering: start time then track.
	prev := Entry{}
	for _, e := range read.Entries {
		if e.StartUS < prev.StartUS {
			t.Fatalf("unordered: %+v after %+v", e, prev)
		}
		prev = e
	}
}

func TestBuilderDuplicateRejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewBuilder(Options{SegmentUS: 1000, OutDir: dir})
	rep := Representation{ID: "a", TrackID: 1}
	tr := mkTrack(1, 1000, []int64{0, 500, 1000}, allKeys(3))
	if _, err := b.EntryFor(rep, tr, 0, 1000, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.EntryFor(rep, tr, 0, 1000, 0, 0, true); err == nil {
		t.Fatal("expected duplicate id")
	}
	if err := b.AddRepresentation(Representation{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRepresentation(Representation{ID: "a"}); err == nil {
		t.Fatal("expected duplicate rep")
	}
	if _, err := b.EntryFor(rep, tr, 1000, 1000, 0, 0, true); err == nil {
		t.Fatal("expected zero-duration error")
	}
	if _, err := b.EntryFor(rep, mkTrack(1, 1000, []int64{5000}, allKeys(1)), 0, 1000, 0, 0, true); err == nil {
		t.Fatal("expected no-samples error")
	}
}

func TestCompositeEntries(t *testing.T) {
	span := int64(4_000_000)
	reps, tracks, idx, al := setupLadder(t, span)
	dir := t.TempDir()
	b, _ := NewBuilder(Options{SegmentUS: 2_000_000, OutDir: dir})
	b.AddRepresentation(Representation{ID: "v1080", TrackID: 1})
	b.AddRepresentation(Representation{ID: "v720", TrackID: 2})
	transitions := []model.Transition{{StartUS: 1_500_000, EndUS: 2_500_000, Kind: model.TransitionCross, Sources: []int{1, 2}}}
	simple, composite, err := b.AlignEntries(al, reps, tracks, idx, transitions)
	if err != nil {
		t.Fatal(err)
	}
	// The transition [1.5s,2.5s) spans the 2s boundary, so both adjacent
	// segments on both representations become composite fragments.
	if len(composite) != 4 {
		t.Fatalf("composite=%d", len(composite))
	}
	for _, c := range composite {
		if c.Kind != model.SegmentComposite || len(c.SourceTrackIDs) != 2 {
			t.Fatalf("bad composite: %+v", c)
		}
		if c.TransitionKind != string(model.TransitionCross) {
			t.Fatalf("transition kind=%s", c.TransitionKind)
		}
	}
	if len(simple) != 0 {
		t.Fatalf("simple=%d", len(simple))
	}
}

func TestManifestVerifyHash(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.m4s")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyHash(file, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 8 || got[:7] != "sha256:" {
		t.Fatalf("hash=%q", got)
	}
	if _, err := VerifyHash(file, got); err != nil {
		t.Fatalf("matching hash rejected: %v", err)
	}
	if _, err := VerifyHash(file, "sha256:deadbeef"); err == nil {
		t.Fatal("expected mismatch")
	}
	if _, err := VerifyHash(filepath.Join(dir, "missing"), ""); err == nil {
		t.Fatal("expected missing file error")
	}
}

func TestReadRejectsBadManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(dir); err == nil {
		t.Fatal("expected missing manifest error")
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("expected corrupt manifest error")
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("expected version error")
	}
}

func TestAbsPathEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := AbsPath(dir, "v720/x.m4s"); err != nil {
		t.Fatal(err)
	}
	if _, err := AbsPath(dir, "../evil.m4s"); err == nil {
		t.Fatal("expected escape error")
	}
	if _, err := AbsPath(dir, "/etc/passwd"); err == nil {
		t.Fatal("expected absolute path error")
	}
	fe, ok := fault.As(mustErr(AbsPath(dir, "../../x")))
	if !ok || fe.Kind != fault.KindPathEscape {
		t.Fatalf("expected path escape fault, got %v", fe)
	}
}

func mustErr(_ string, err error) error { return err }

func TestPlanFromEntry(t *testing.T) {
	tr := mkTrack(1, 1000, []int64{0, 500, 1000, 1500}, allKeys(4))
	e := Entry{ID: "s", SampleNumbers: []int{1, 2}}
	samples, err := PlanFromEntry(e, tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[1].Number != 2 {
		t.Fatalf("samples=%v", samples)
	}
	if _, err := PlanFromEntry(Entry{ID: "s", SampleNumbers: []int{9}}, tr); err == nil {
		t.Fatal("expected missing sample error")
	}
	if _, err := PlanFromEntry(e, nil); err == nil {
		t.Fatal("expected nil track error")
	}
}

func TestAddIssueAndOptions(t *testing.T) {
	if _, err := NewBuilder(Options{OutDir: "/tmp"}); err == nil {
		t.Fatal("expected segment length error")
	}
	if _, err := NewBuilder(Options{SegmentUS: 1}); err == nil {
		t.Fatal("expected outdir error")
	}
	b, err := NewBuilder(Options{SegmentUS: 1, OutDir: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	b.AddIssue("")
	b.AddIssue("recovered tail")
	m := b.Build(1000, nil)
	if len(m.Issues) != 1 || m.Issues[0] != "recovered tail" {
		t.Fatalf("issues=%v", m.Issues)
	}
}
