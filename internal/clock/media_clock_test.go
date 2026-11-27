package clock

import "testing"

func TestObserveTfdtTimescaleNormalization(t *testing.T) {
	m := New()
	if err := m.AddTrack(TrackConfig{TrackID: 1, Timescale: 90000, Kind: "video"}); err != nil {
		t.Fatal(err)
	}
	// 90000 ticks == 1000 ms on a 90kHz track.
	obs, err := m.ObserveTfdt(1, 90000)
	if err != nil || obs.MasterMS != 1000 {
		t.Fatalf("obs = %+v err=%v", obs, err)
	}
	if obs.Confidence != Confirmed {
		t.Fatalf("first monotonic obs should be confirmed, got %v", obs.Confidence)
	}
}

func TestObserveTfdt33BitWrap(t *testing.T) {
	m := New()
	_ = m.AddTrack(TrackConfig{TrackID: 2, Timescale: 1000, Kind: "audio"})
	const mod = uint64(1) << 33
	near := mod - 500
	obs1, _ := m.ObserveTfdt(2, near)
	obs2, err := m.ObserveTfdt(2, 500) // wraps forward
	if err != nil {
		t.Fatalf("wrap must be repaired, not rejected: %v", err)
	}
	if obs2.Wraps != 1 || obs2.ExtendedTfdt != mod+500 {
		t.Fatalf("wrap ext = %+v", obs2)
	}
	if obs1.Confidence == Rejected || obs2.Confidence == Rejected {
		t.Fatal("wrap repair must not mark rejected")
	}
}

func TestObserveTfdtRollbackRejects(t *testing.T) {
	m := New()
	_ = m.AddTrack(TrackConfig{TrackID: 3, Timescale: 48000})
	if _, err := m.ObserveTfdt(3, 100000); err != nil {
		t.Fatal(err)
	}
	obs, err := m.ObserveTfdt(3, 90000) // genuine rollback, no wrap
	if err == nil || obs.Confidence != Rejected {
		t.Fatalf("rollback must reject: obs=%+v err=%v", obs, err)
	}
	if !HasKind(err, ErrTfdtRollback) || !m.IsBlocked(3) {
		t.Fatalf("error kind / blocked state wrong: %v", err)
	}
	if len(m.Errors()) != 1 {
		t.Fatalf("errors = %v", m.Errors())
	}
}

func TestUnknownAndBadTrack(t *testing.T) {
	m := New()
	if _, err := m.ObserveTfdt(99, 1); !HasKind(err, ErrTrackUnknown) {
		t.Fatalf("unknown track: %v", err)
	}
	if err := m.AddTrack(TrackConfig{TrackID: 1, Timescale: 0}); !HasKind(err, ErrTimescaleInvalid) {
		t.Fatalf("zero timescale: %v", err)
	}
	if err := m.AddTrack(TrackConfig{TrackID: 1, Timescale: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddTrack(TrackConfig{TrackID: 1, Timescale: 1000}); !HasKind(err, ErrTimescaleInvalid) {
		t.Fatalf("duplicate track must error: %v", err)
	}
}

func TestProjectSamplesVFR(t *testing.T) {
	m := New()
	_ = m.AddTrack(TrackConfig{TrackID: 1, Timescale: 1000})
	// VFR durations on a 1kHz track: 10,30,20 ms — all monotonic.
	pts, err := m.ProjectSamples(1, 0, []uint32{10, 30, 20})
	if err != nil {
		t.Fatal(err)
	}
	wantStart := []int64{0, 10, 40}
	for i, p := range pts {
		if p.StartMS != wantStart[i] {
			t.Fatalf("sample %d start = %d want %d", i, p.StartMS, wantStart[i])
		}
	}
}

func TestProjectSamplesZeroDurationMonotonic(t *testing.T) {
	m := New()
	_ = m.AddTrack(TrackConfig{TrackID: 1, Timescale: 1000})
	// Strict-monotonic start times: a zero-duration sample is accepted as
	// long as the next sample starts at/after its end (no backwards move).
	pts, err := m.ProjectSamples(1, 100, []uint32{50, 0, 25})
	if err != nil {
		t.Fatalf("zero-duration sample rejected: %v", err)
	}
	if pts[1].StartMS != 150 || pts[2].StartMS != 150 {
		t.Fatalf("zero-duration geometry wrong: %+v", pts)
	}
	if m.IsBlocked(1) {
		t.Fatal("valid VFR projection must not block track")
	}
}

func TestProjectSamplesOverlapAcrossFragments(t *testing.T) {
	m := New()
	_ = m.AddTrack(TrackConfig{TrackID: 1, Timescale: 1000})
	// First fragment occupies [100,200) ms.
	if _, err := m.ProjectSamples(1, 100, []uint32{100}); err != nil {
		t.Fatal(err)
	}
	// Second fragment's base falls inside the already-presented range:
	// the clock must surface a non-monotonic error rather than overwrite.
	_, err := m.ProjectSamples(1, 150, []uint32{50})
	if !HasKind(err, ErrNonMonotonicSamples) {
		t.Fatalf("overlapping fragment must error non-monotonic: %v", err)
	}
	if !m.IsBlocked(1) {
		t.Fatal("track must be blocked after overlapping fragment")
	}
}

func TestTicksRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		ms        int64
		timescale uint32
	}{{1000, 90000}, {33, 48000}, {1, 1000}} {
		ticks := MSToTicks(tc.ms, tc.timescale)
		back := ticksToMS(ticks, tc.timescale)
		if back != tc.ms {
			t.Fatalf("roundtrip %dms @%d = %d ticks -> %dms", tc.ms, tc.timescale, ticks, back)
		}
	}
}
