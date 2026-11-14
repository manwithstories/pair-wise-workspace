package align

import (
	"testing"

	"dashpack/internal/ingest"
)

func mkTrack(id string, ts, pts0 int64, durs []int64) ingest.Track {
	tr := ingest.Track{ID: id, StartNumber: 1, Bandwidth: 1}
	pts := pts0
	for _, d := range durs {
		tr.Segments = append(tr.Segments, ingest.SegmentMeta{Timescale: ts, PTS: pts, Duration: d})
		pts += d
	}
	return tr
}

func TestAlignSnapsToCommonGrid(t *testing.T) {
	// 4s segments at 48000 and 90000 with +/-2ms jitter on inner boundaries.
	a := mkTrack("r1", 48000, 96000, []int64{191904, 192096, 192000})
	b := mkTrack("r2", 90000, 180000, []int64{359820, 360180, 360000})
	res, err := Align([]ingest.Track{a, b}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reps) != 2 {
		t.Fatalf("reps = %d", len(res.Reps))
	}
	for _, r := range res.Reps {
		for i, c := range r.Cuts {
			want := r.PTO + int64(i)*360000
			if c != want {
				t.Errorf("%s cut %d = %d, want %d", r.Track.ID, i, c, want)
			}
		}
		if r.MaxSnapDev > 200 { // ~2.2ms in 90000 ticks
			t.Errorf("%s snap dev %d too large", r.Track.ID, r.MaxSnapDev)
		}
	}
	if res.DurationCommon != 3*360000 {
		t.Errorf("duration = %d", res.DurationCommon)
	}
}

func TestAlignRejectsRegression(t *testing.T) {
	tr := mkTrack("r1", 90000, 0, []int64{100, 100})
	tr.Segments[1].PTS = 50 // goes backwards
	if _, err := Align([]ingest.Track{tr}, 4); err == nil {
		t.Fatal("want regression error")
	}
}

func TestAlignMidStreamTimescale(t *testing.T) {
	tr := mkTrack("r1", 48000, 0, []int64{192000, 192000})
	tr.Segments = append(tr.Segments,
		ingest.SegmentMeta{Timescale: 16000, PTS: 384000 / 3, Duration: 64000})
	res, err := Align([]ingest.Track{tr}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Reps[0].Cuts[3]; got != 3*360000 {
		t.Fatalf("end cut = %d, want %d", got, 3*360000)
	}
}
