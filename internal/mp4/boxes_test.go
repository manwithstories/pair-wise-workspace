package mp4_test

import (
	"testing"

	"mediaprobe/internal/fixture"
	"mediaprobe/internal/mp4"
)

func gen(t *testing.T, mode fixture.CorruptMode) *fixture.Stream {
	t.Helper()
	return fixture.Generate(fixture.Options{Seed: 7, Fragments: 8, VFR: true, Corrupt: mode})
}

func parseInit(t *testing.T, s *fixture.Stream) *mp4.InitInfo {
	t.Helper()
	init, err := mp4.ParseInit(s.Init)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	return init
}

func TestParseInitTracks(t *testing.T) {
	init := parseInit(t, gen(t, fixture.CorruptNone))
	v, ok := init.Track(fixture.VideoTrackID)
	if !ok || v.Timescale != fixture.VideoTimescale {
		t.Fatalf("video track: %+v ok=%v", v, ok)
	}
	a, ok := init.Track(fixture.AudioTrackID)
	if !ok || a.Timescale != fixture.AudioTimescale {
		t.Fatalf("audio track: %+v ok=%v", a, ok)
	}
}

func TestParseFragmentSamples(t *testing.T) {
	s := gen(t, fixture.CorruptNone)
	init := parseInit(t, s)
	f, err := mp4.ParseFragment(s.Fragments[0], init)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if f.Seq != 1 || f.TrackID != fixture.VideoTrackID || f.TFDT != 0 {
		t.Fatalf("seq=%d track=%d tfdt=%d", f.Seq, f.TrackID, f.TFDT)
	}
	if len(f.Samples) != 30 {
		t.Fatalf("samples=%d", len(f.Samples))
	}
	if !f.Samples[0].Keyframe {
		t.Fatal("first sample must be a keyframe")
	}
	var dur int64
	for _, sm := range f.Samples {
		dur += int64(sm.Duration)
		if len(sm.Data) != int(sm.Size) {
			t.Fatalf("sample %d data/size mismatch", sm.Index)
		}
	}
	if dur != 90000 {
		t.Fatalf("duration sum=%d", dur)
	}
}

func TestTruncatedBoxReportsOffset(t *testing.T) {
	s := gen(t, fixture.CorruptTruncatedBox)
	init := parseInit(t, s)
	var corrupt *mp4.CorruptError
	parsed := 0
	for _, raw := range s.Fragments {
		_, err := mp4.ParseFragment(raw, init)
		if err == nil {
			parsed++
			continue
		}
		ce, ok := mp4.AsCorrupt(err)
		if !ok {
			t.Fatalf("non-corrupt error: %v", err)
		}
		if corrupt != nil {
			t.Fatal("more than one corrupt fragment")
		}
		corrupt = ce
	}
	if corrupt == nil || corrupt.Offset <= 0 {
		t.Fatalf("no corrupt offset reported: %+v", corrupt)
	}
	if parsed != len(s.Fragments)-1 {
		t.Fatalf("parsed=%d want %d (isolation must not drop later fragments)", parsed, len(s.Fragments)-1)
	}
}

func TestSampleRangeReportsTrunOffset(t *testing.T) {
	s := gen(t, fixture.CorruptSampleRange)
	init := parseInit(t, s)
	found := false
	for _, raw := range s.Fragments {
		_, err := mp4.ParseFragment(raw, init)
		if ce, ok := mp4.AsCorrupt(err); ok {
			found = true
			if ce.Offset <= 0 {
				t.Fatalf("offset=%d", ce.Offset)
			}
		}
	}
	if !found {
		t.Fatal("out-of-range sample not detected")
	}
}

func TestSeqTrackerGapsDupsOutOfOrder(t *testing.T) {
	st := mp4.NewSeqTracker()
	for _, seq := range []uint32{1, 2, 4, 3, 3, 7} {
		st.Observe(seq)
	}
	if len(st.Duplicates) != 1 || st.Duplicates[0] != 3 {
		t.Fatalf("dups=%v", st.Duplicates)
	}
	if len(st.OutOfOrder) != 1 || st.OutOfOrder[0] != 3 {
		t.Fatalf("out-of-order=%v", st.OutOfOrder)
	}
	if st.Missing() != 2 { // 5 and 6 still missing
		t.Fatalf("missing=%d", st.Missing())
	}
}

func TestRoundtripValidFragments(t *testing.T) {
	s := gen(t, fixture.CorruptNone)
	init := parseInit(t, s)
	var rt mp4.RoundtripResult
	for _, raw := range s.Fragments {
		f, err := mp4.ParseFragment(raw, init)
		if err != nil {
			t.Fatalf("ParseFragment: %v", err)
		}
		mp4.RoundtripFragment(f, &rt)
	}
	if rt.Percent() != 100 || rt.FirstDiff != "" {
		t.Fatalf("roundtrip=%.1f firstDiff=%s", rt.Percent(), rt.FirstDiff)
	}
}
