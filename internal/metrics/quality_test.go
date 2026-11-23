package metrics

import (
	"strings"
	"testing"
)

func TestFromSourceComputesBitrateAndDelta(t *testing.T) {
	// 10 s of 1.5 MB is 1200 kbps, exactly the target.
	src := SourceBytes{Bytes: 1_500_000, DurationSec: 10}
	actual, delta := FromSource(src, 1200)
	if actual != 1200 {
		t.Fatalf("actual = %.3f, want 1200", actual)
	}
	if delta != 0 {
		t.Fatalf("delta = %.3f, want 0", delta)
	}
	// Target 1000: +20%.
	_, delta = FromSource(src, 1000)
	if delta != 20 {
		t.Fatalf("delta = %.3f, want 20", delta)
	}
}

func TestFromSourceRejectsEmpty(t *testing.T) {
	if a, d := FromSource(SourceBytes{}, 1000); a != 0 || d != 0 {
		t.Fatalf("got %.3f %.3f, want zeros", a, d)
	}
}

func TestQualityStats(t *testing.T) {
	q := QualityStats([]float64{0.5, 0.7, 0.9}, 2)
	if q.Count != 3 || q.Bad != 2 {
		t.Fatalf("%+v", q)
	}
	if q.Mean < 0.699 || q.Mean > 0.701 {
		t.Fatalf("mean = %f", q.Mean)
	}
	if q.Min != 0.5 || q.Max != 0.9 {
		t.Fatalf("min/max = %f/%f", q.Min, q.Max)
	}
}

func TestSkewFromManifestRecomputed(t *testing.T) {
	// Twenty perfectly spaced rows: skew is zero.
	var rows []ManifestRow
	for i := 0; i < 20; i++ {
		rows = append(rows, ManifestRow{Track: 1, Frame: uint64(i + 1), PTS: int64(i) * 90})
	}
	skew, err := SkewFromManifest(rows, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if skew != 0 {
		t.Fatalf("skew = %f, want 0", skew)
	}
	// Nudge one row by more than half a step: skew must be caught.
	rows[10].PTS += 45
	skew, err = SkewFromManifest(rows, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if skew <= 0 {
		t.Fatalf("skew = %f, want > 0 after nudge", skew)
	}
}

func TestReadManifestSkipsHeader(t *testing.T) {
	csv := "track,frame,pts,offset,size,content_sha256,status\n1,1,0,0,10,aa,ok\n1,2,90,10,10,bb,ok\n"
	rows, err := ReadManifest(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].PTS != 90 || rows[0].Frame != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestReadManifestRejectsBadRows(t *testing.T) {
	csv := "track,frame,pts\nx,1,0\n"
	if _, err := ReadManifest(strings.NewReader(csv)); err == nil {
		t.Fatal("accepted non-numeric track")
	}
}

func TestCheckThresholds(t *testing.T) {
	ok := TrackMetrics{Track: 1, FramesBad: 2, BitrateDeltaPct: 4.9, ManifestSkewMS: 0.99}
	if err := CheckThresholds(ok); err != nil {
		t.Fatalf("valid metrics rejected: %v", err)
	}
	for _, bad := range []TrackMetrics{
		{Track: 1, FramesBad: 4},
		{Track: 1, BitrateDeltaPct: 6},
		{Track: 1, BitrateDeltaPct: -6},
		{Track: 1, ManifestSkewMS: 1.5},
	} {
		if err := CheckThresholds(bad); err == nil {
			t.Fatalf("accepted out-of-bounds metrics %+v", bad)
		}
	}
}

func TestFileSize(t *testing.T) {
	if _, err := FileSize("/nonexistent/file"); err == nil {
		t.Fatal("FileSize accepted a missing file")
	}
}
