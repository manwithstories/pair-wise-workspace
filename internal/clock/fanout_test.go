package clock

import (
	"testing"

	"mediad/internal/media"
)

type recv struct {
	id string
	n  int
}

func (r *recv) ClockID() string { return r.id }
func (r *recv) ObserveSharedClock(c media.Clock, p []Point) error {
	r.n += len(p)
	return nil
}

func TestFanoutConvertsTracksToSharedClock(t *testing.T) {
	v := &media.Track{ID: 1, Timescale: 90_000, Samples: []media.Sample{{Index: 1, PTS: 90_000, DTS: 90_000}}}
	a := &media.Track{ID: 2, Timescale: 48_000, Samples: []media.Sample{{Index: 1, PTS: 48_000, DTS: 48_000}}}
	f, faults := New(&media.Project{Tracks: []*media.Track{v, a}}, 90_000)
	if len(faults) != 0 {
		t.Fatalf("faults=%#v", faults)
	}
	if f.Tracks[1].Points[0].PTS != 90_000 || f.Tracks[2].Points[0].PTS != 90_000 {
		t.Fatalf("shared pts video=%d audio=%d", f.Tracks[1].Points[0].PTS, f.Tracks[2].Points[0].PTS)
	}
	if f.Tracks[1].Points[0].PTSNS != 1_000_000_000 {
		t.Fatalf("ns=%d", f.Tracks[1].Points[0].PTSNS)
	}
	r1, r2 := &recv{id: "ladder"}, &recv{id: "recovery"}
	if ff := f.Dispatch(r1, r2); len(ff) != 0 {
		t.Fatalf("dispatch faults=%#v", ff)
	}
	if r1.n != 2 || r2.n != 2 {
		t.Fatalf("receiver counts %d %d", r1.n, r2.n)
	}
}

func TestBoundaryGapIsSmallForStandardClocks(t *testing.T) {
	// 30 fps video and 48 kHz / 1024-sample audio.
	v := &media.Track{ID: 1, Timescale: 90_000}
	for i := 0; i < 60; i++ {
		v.Samples = append(v.Samples, media.Sample{Index: uint32(i + 1), PTS: int64(i) * 3000, DTS: int64(i) * 3000})
	}
	a := &media.Track{ID: 2, Timescale: 48_000}
	for i := 0; i < 100; i++ {
		a.Samples = append(a.Samples, media.Sample{Index: uint32(i + 1), PTS: int64(i) * 1024, DTS: int64(i) * 1024})
	}
	f, _ := New(&media.Project{Tracks: []*media.Track{v, a}}, 90_000)
	gap := f.BoundaryGapNS(1, 2)
	if gap > 20_000_000 {
		t.Fatalf("gap=%dns exceeds 20ms", gap)
	}
}
