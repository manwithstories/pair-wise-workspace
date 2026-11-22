package manifest

import (
	"bytes"
	"strconv"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/timeline"
)

func track(num uint64, startMS, endMS float64) *timeline.TrackInfo {
	info := &timeline.TrackInfo{Number: num, Kind: "video"}
	start := clock.MustParseDecimal(fmtFloat(startMS / 1000.0))
	end := clock.MustParseDecimal(fmtFloat(endMS / 1000.0))
	info.Frames = []timeline.Frame{
		{Track: num, Offset: 100, Original: start, Repaired: start, Keyframe: true},
		{Track: num, Offset: 200, Original: end, Repaired: end},
	}
	return info
}

func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func TestGenerateAndVerifyWithinTolerance(t *testing.T) {
	tracks := map[uint64]*timeline.TrackInfo{
		1: track(1, 0, 120),
		2: track(2, 0, 120),
	}
	m := Generate(tracks, declaredFor(tracks, 1, 2), "digest-abc")
	if len(m.Tracks) != 2 || m.FrameCount != 4 {
		t.Fatalf("manifest=%+v", m)
	}
	if m.DurationUsec != 120_000 {
		t.Fatalf("duration=%d", m.DurationUsec)
	}
	al := Verify(m, tracks)
	if !al.OK || al.MaxErrorUsec != 0 {
		t.Fatalf("alignment=%+v faults=%v", al, al.Faults.Faults())
	}
}

func TestDriftBeyondOneMillisecondRejected(t *testing.T) {
	tracks := map[uint64]*timeline.TrackInfo{1: track(1, 0, 120)}
	declared := declaredFor(tracks, 1)
	m := Generate(tracks, declared, "d")
	// Perturb the manifest's declared end by 2ms.
	m.Tracks[0].DeclaredEndUsec += 2000
	al := Verify(m, tracks)
	if al.OK {
		t.Fatal("2ms drift must be rejected")
	}
	if al.MaxErrorUsec != 2000 || al.MaxErrorMs != 2.0 {
		t.Fatalf("max error=%d us (%.3f ms)", al.MaxErrorUsec, al.MaxErrorMs)
	}
	if al.Faults.Count(fault.KindManifestDrift) == 0 {
		t.Fatalf("faults=%v", al.Faults.Faults())
	}
	// The fault must carry the block offset for localization.
	f := al.Faults.Faults()[0]
	if !f.HasOffset || int64(f.Offset.Value) != m.Tracks[0].EndOffset {
		t.Fatalf("fault offset=%v want %d", f.Offset, m.Tracks[0].EndOffset)
	}
	// A drift is diagnosed damage (exit 2); it blocks commit, it is not an
	// infrastructure failure.
	if al.Faults.ExitCode() != fault.ExitDamaged {
		t.Fatalf("exit=%d", al.Faults.ExitCode())
	}
}

func TestExactlyOneMillisecondAccepted(t *testing.T) {
	tracks := map[uint64]*timeline.TrackInfo{1: track(1, 0, 120)}
	declared := declaredFor(tracks, 1)
	m := Generate(tracks, declared, "d")
	m.Tracks[0].DeclaredStartUsec += 1000
	al := Verify(m, tracks)
	if !al.OK || al.MaxErrorUsec != 1000 {
		t.Fatalf("1ms boundary: ok=%v max=%d", al.OK, al.MaxErrorUsec)
	}
	m.Tracks[0].DeclaredStartUsec += 1
	if al := Verify(m, tracks); al.OK {
		t.Fatal("1001us must fail")
	}
}

func TestMissingTrackFails(t *testing.T) {
	m := Manifest{Version: 1, Tracks: []TrackRange{{Track: 9}}}
	al := Verify(m, map[uint64]*timeline.TrackInfo{})
	if al.OK || al.Faults.Count(fault.KindManifestDrift) != 1 {
		t.Fatalf("alignment=%+v", al)
	}
}

func TestManifestJSONDeterministic(t *testing.T) {
	tracks := map[uint64]*timeline.TrackInfo{1: track(1, 0, 120), 2: track(2, 40, 160)}
	m := Generate(tracks, declaredFor(tracks, 2, 1), "d")
	var a, b bytes.Buffer
	_ = m.WriteJSON(&a)
	_ = m.WriteJSON(&b)
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("manifest JSON not deterministic")
	}
	// Sorted by track number regardless of input order.
	if m.Tracks[0].Track != 1 {
		t.Fatal("tracks not sorted")
	}
}

func TestToleranceMillis(t *testing.T) {
	if got := ToleranceMillis(); got != 1.0 {
		t.Fatalf("tolerance=%v", got)
	}
}

// declaredFor builds declared extents that match the tracks' repaired extents
// plus one default frame, so a fresh manifest verifies cleanly.
func declaredFor(tracks map[uint64]*timeline.TrackInfo, order ...uint64) []Declared {
	var out []Declared
	for _, n := range order {
		info := tracks[n]
		start, end := info.StartEnd()
		if info.DefaultNS > 0 {
			end = end.Add(clock.MustNew(int64(info.DefaultNS), 1_000_000_000))
		}
		out = append(out, Declared{Track: n, Kind: info.Kind, Start: start, End: end})
	}
	return out
}
