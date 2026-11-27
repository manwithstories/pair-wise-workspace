package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/meta"
	"mediacore/internal/scan"
)

func sampleScan() *scan.Scan {
	return &scan.Scan{
		Path: "input.mp4", Format: "mp4", DurationUS: 3_000_000,
		Tracks: []scan.Track{
			{Index: 0, Kind: scan.TrackVideo, Codec: "h264", TimeBase: clock.Rational{Num: 1, Den: 1000}, Start: 0},
			{Index: 1, Kind: scan.TrackAudio, Codec: "aac", TimeBase: clock.Rational{Num: 1, Den: 48000}, Start: 0, SampleRate: 48000, Channels: 2},
		},
		Keyframes: []scan.Keyframe{
			{Track: 0, PTS: 0, TimeUS: 0, BytePos: 0, Size: 100},
			{Track: 0, PTS: 2000, TimeUS: 2_000_000, BytePos: 5000, Size: 120},
		},
		Meta: []scan.MetaRecord{
			{Kind: meta.KindSubtitle, Track: 1, TimeUS: 500_000, Payload: []byte("hello"), Text: "hello"},
			{Kind: meta.KindID3, Track: 1, TimeUS: 1_500_000, Payload: []byte("ID3")},
			{Kind: meta.KindSEI, Track: 0, TimeUS: 2_500_000, Quarantined: true, Reason: "bad"},
		},
		Chapters: []meta.Chapter{{ID: 1, Title: "Intro", StartUs: 0, EndUs: 1_500_000}},
	}
}

func testDigest(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = 'a' + (c+byte(i))%6
	}
	return string(b)
}

func TestBuildIndexesAndSkipsQuarantine(t *testing.T) {
	ix, err := Build(sampleScan(), testDigest(0), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Records) != 2 {
		t.Fatalf("quarantined record indexed: %d", len(ix.Records))
	}
	for _, r := range ix.Records {
		if r.Kind == meta.KindSEI {
			t.Fatal("quarantined SEI leaked into index")
		}
	}
	if len(ix.Keyframes) != 2 || len(ix.Chapters) != 1 {
		t.Fatalf("counts: kf=%d ch=%d", len(ix.Keyframes), len(ix.Chapters))
	}
	if err := ix.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRejectsBadDigest(t *testing.T) {
	if _, err := Build(sampleScan(), "short", 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short digest accepted: %v", err)
	}
}

func TestValidateRejectsCorruption(t *testing.T) {
	ix, _ := Build(sampleScan(), testDigest(1), 10)
	cases := []func(*Index){
		func(i *Index) { i.Version = 99 },
		func(i *Index) { i.Keyframes[0].Track = 42 },
		func(i *Index) { i.Records[0].TimeUS = -1 },
		func(i *Index) { i.Chapters[0].EndUS = -5 },
		func(i *Index) { i.TrackList[0].TimeBase = clock.Rational{} },
	}
	for n, mutate := range cases {
		cp := *ix
		cp.Keyframes = append([]Keyframe(nil), ix.Keyframes...)
		cp.Records = append([]Record(nil), ix.Records...)
		cp.Chapters = append([]Chapter(nil), ix.Chapters...)
		cp.TrackList = append([]Track(nil), ix.TrackList...)
		mutate(&cp)
		if err := cp.Validate(); err == nil {
			t.Fatalf("case %d accepted corrupt index", n)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	dg := testDigest(2)
	ix, _ := Build(sampleScan(), dg, 99)
	if err := st.Save(ix); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(dg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != dg || len(got.Records) != 2 {
		t.Fatalf("roundtrip: %+v", got.Summarize())
	}
	// Load for a different digest must report stale/missing, never return data.
	if _, err := st.Load(testDigest(3)); !errors.Is(err, ErrMissing) {
		t.Fatalf("other digest: %v", err)
	}
}

func TestStoreDetectsStaleDigest(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	ix, _ := Build(sampleScan(), testDigest(4), 1)
	if err := st.Save(ix); err != nil {
		t.Fatal(err)
	}
	// Simulate a cache file whose recorded digest differs from its filename,
	// e.g. after a partial manual copy.
	raw, _ := os.ReadFile(st.path(ix.Digest))
	other := testDigest(5)
	if err := os.WriteFile(st.path(other), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(other); !errors.Is(err, ErrStale) {
		t.Fatalf("expected stale, got %v", err)
	}
}

func TestEnsureRebuildsOnMissingAndStale(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	dg := testDigest(6)
	calls := 0
	build := func() (*scan.Scan, error) {
		calls++
		return sampleScan(), nil
	}
	res, err := st.Ensure(dg, 10, build)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rebuilt || calls != 1 {
		t.Fatalf("first ensure: rebuilt=%v calls=%d", res.Rebuilt, calls)
	}
	res, err = st.Ensure(dg, 10, build)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rebuilt || calls != 1 {
		t.Fatalf("second ensure should hit cache: rebuilt=%v calls=%d", res.Rebuilt, calls)
	}
	// Corrupt the cache and rebuild.
	raw := []byte("{not json")
	if err := os.WriteFile(st.path(dg), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = st.Ensure(dg, 10, build)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rebuilt || calls != 2 {
		t.Fatalf("corrupt cache not rebuilt: %+v calls=%d", res, calls)
	}
}

func TestEnsureChainsErrors(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	boom := errors.New("boom")
	if _, err := st.Ensure(testDigest(7), 0, func() (*scan.Scan, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("build error lost: %v", err)
	}
}

func TestKeyframeBeforeAndRecordsBetween(t *testing.T) {
	ix, _ := Build(sampleScan(), testDigest(8), 1)
	k, ok := ix.KeyframeBefore(0, 2_100_000)
	if !ok || k.TimeUS != 2_000_000 {
		t.Fatalf("keyframe: %+v %v", k, ok)
	}
	if k, ok := ix.KeyframeBefore(0, 1); !ok || k.TimeUS != 0 {
		t.Fatal("expected track start keyframe at t=0")
	}
	if _, ok := ix.KeyframeBefore(1, 1_000_000); ok {
		t.Fatal("keyframe returned for a track without keyframes")
	}
	if _, ok := ix.KeyframeBefore(9, 1000); ok {
		t.Fatal("keyframe for unknown track")
	}
	recs := ix.RecordsBetween(0, 1_000_000)
	if len(recs) != 1 || recs[0].Text != "hello" {
		t.Fatalf("records: %+v", recs)
	}
}

func TestAtomicSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	ix, _ := Build(sampleScan(), testDigest(9), 1)
	if err := st.Save(ix); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestInvalidateStaleSamePathDifferentDigest(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	oldDigest := testDigest(11)
	freshDigest := testDigest(12)
	sc := sampleScan()
	sc.Path = "/media/edge.mp4"

	old, _ := Build(sc, oldDigest, 1)
	if err := st.Save(old); err != nil {
		t.Fatal(err)
	}
	removed, err := st.InvalidateStale("/media/edge.mp4", freshDigest)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d stale entries, want 1", removed)
	}
	if _, err := st.Load(oldDigest); !errors.Is(err, ErrMissing) {
		t.Fatalf("stale index still readable: %v", err)
	}
	// An entry for a different path must be left alone.
	other := sampleScan()
	other.Path = "/media/other.mp4"
	oix, _ := Build(other, testDigest(13), 1)
	if err := st.Save(oix); err != nil {
		t.Fatal(err)
	}
	removed, err = st.InvalidateStale("/media/edge.mp4", freshDigest)
	if err != nil || removed != 0 {
		t.Fatalf("unrelated path removed: %d %v", removed, err)
	}
	if _, err := st.Load(testDigest(13)); err != nil {
		t.Fatalf("unrelated index lost: %v", err)
	}
}

func TestInvalidateAndList(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	dg := testDigest(10)
	ix, _ := Build(sampleScan(), dg, 1)
	if err := st.Save(ix); err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil || len(list) != 1 || list[0] != dg {
		t.Fatalf("list: %v %v", list, err)
	}
	if err := st.Invalidate(dg); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(dg); !errors.Is(err, ErrMissing) {
		t.Fatalf("after invalidate: %v", err)
	}
	// Invalidating a missing entry is not an error.
	if err := st.Invalidate(dg); err != nil {
		t.Fatal(err)
	}
}
