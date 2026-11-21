package stream

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/fault"
	"mediacheck/internal/timeline"
)

func scanFile(t *testing.T, path string) *MP4Info {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	return ScanMP4(f, st.Size())
}

func TestScanMP4Normal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.mp4")
	spec := fixture.DefaultMP4Spec()
	wantKeys, err := fixture.WriteMP4(p, spec)
	if err != nil {
		t.Fatal(err)
	}
	info := scanFile(t, p)
	if info.Faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", info.Faults.Errs)
	}
	if len(info.Tracks) != 1 {
		t.Fatalf("want 1 track, got %d", len(info.Tracks))
	}
	tr := info.Tracks[0]
	if tr.Timescale != 90000 || tr.SampleCount != 120 || !tr.IndexOK {
		t.Errorf("track: %+v", tr)
	}
	if len(tr.Keyframes) != len(wantKeys) {
		t.Fatalf("keyframes: got %d want %d", len(tr.Keyframes), len(wantKeys))
	}
	for i, k := range tr.Keyframes {
		if k != wantKeys[i] {
			t.Errorf("keyframe %d: got %d want %d", i, k, wantKeys[i])
		}
	}
	if info.TrustedBytes != info.TotalBytes {
		t.Errorf("trusted %d != total %d", info.TrustedBytes, info.TotalBytes)
	}
}

func TestScanMP4Truncated(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.mp4")
	if _, err := fixture.WriteMP4(full, fixture.DefaultMP4Spec()); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(full)
	trunc := filepath.Join(dir, "trunc.mp4")
	cut := st.Size() - 30 // chop into the stco payload
	if err := fixture.TruncateFile(full, trunc, cut); err != nil {
		t.Fatal(err)
	}
	info := scanFile(t, trunc)
	if info.Faults.Len() == 0 {
		t.Fatal("truncation must be reported")
	}
	if info.TrustedBytes >= info.TotalBytes {
		t.Errorf("trusted range should shrink: %d of %d",
			info.TrustedBytes, info.TotalBytes)
	}
	for _, e := range info.Faults.Errs {
		if !e.Recoverable {
			t.Errorf("truncation should be recoverable: %v", e)
		}
		if e.Offset < 0 {
			t.Errorf("fault missing offset: %v", e)
		}
	}
}

func TestRTPReorderAndGaps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rtp.bin")
	if err := fixture.WriteRTP(p, false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	rec := NewReorderer(DefaultWindow)
	fl := faultList()
	ParseRTPStream(data, rec, fl)
	if fl.Len() != 0 {
		t.Fatalf("unexpected faults: %v", fl.Errs)
	}
	stats := rec.Stats()
	if len(stats) != 1 {
		t.Fatalf("want 1 stream, got %d", len(stats))
	}
	s := stats[0]
	// 24 arrivals: 23 unique (seq 102 dropped) + 1 late duplicate.
	if s.Received != 24 {
		t.Errorf("received=%d", s.Received)
	}
	if s.Reordered != 15 { // swap + everything buffered behind the gap
		t.Errorf("reordered=%d", s.Reordered)
	}
	if s.Late != 1 {
		t.Errorf("late=%d", s.Late)
	}
	if s.Gaps != 2 { // dropped seq + late duplicate
		t.Errorf("gaps=%d", s.Gaps)
	}
	if s.Committed != 23 {
		t.Errorf("committed=%d", s.Committed)
	}
}

func TestSegmentsAndHitRate(t *testing.T) {
	keys := []int64{0, 1_000_000_000, 2_000_000_000, 3_000_000_000}
	corrupt := []Range{{Start: 2_500_000_000, End: 3_500_000_000}}
	segs := BuildSegments(keys, 4_000_000_000, corrupt)
	if len(segs) != 4 {
		t.Fatalf("want 4 segments, got %d", len(segs))
	}
	if segs[0].Corrupted || segs[1].Corrupted || !segs[2].Corrupted || !segs[3].Corrupted {
		t.Errorf("corruption mapping wrong: %+v", segs)
	}
	if segs[0].End != segs[1].Start {
		t.Error("segments must be closed-open contiguous")
	}
	if hr := HitRate(keys, keys, 0); hr != 1.0 {
		t.Errorf("hit rate %v", hr)
	}
	if hr := HitRate(keys, keys[:3], 0); hr != 0.75 {
		t.Errorf("hit rate %v", hr)
	}
}

func TestGOPAnomaly(t *testing.T) {
	fl := faultList()
	CheckGOP([]int64{0, 1_000_000_000, 10_000_000_000}, 2_000_000_000, fl)
	if fl.Len() != 1 {
		t.Fatalf("want 1 anomaly, got %v", fl.Errs)
	}
	if got := timeline.WrapDay(10_000_000_000); got != 10_000_000_000 {
		t.Errorf("sanity: %d", got)
	}
}

// faultList is a tiny helper for tests.
func faultList() *fault.List { return &fault.List{} }
