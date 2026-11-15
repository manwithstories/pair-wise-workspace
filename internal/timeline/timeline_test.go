package timeline

import (
	"testing"

	"mp4probe/internal/moov"
)

func mapper() *Mapper {
	elst := []moov.ElstEntry{
		{Duration: 450000, MediaTime: -1, Rate: 1 << 16},        // 5s empty
		{Duration: 40500000, MediaTime: 14400, Rate: 1 << 16},   // normal
		{Duration: 3600000, MediaTime: 40514400, Rate: 1 << 15}, // half rate
	}
	return NewMapper(elst, 90000, 90000, true)
}

func TestMapperEmptyEdit(t *testing.T) {
	m := mapper()
	if _, ok := m.ToMedia(225000); ok {
		t.Fatal("empty edit must not map to media")
	}
	if _, ok := m.ToPresentation(3600); ok {
		t.Fatal("media before segment 2 start must not map")
	}
	if pres, ok := m.ToPresentation(14400 + 3600); !ok || pres != 450000+3600 {
		t.Fatalf("pres = %d ok=%v", pres, ok)
	}
}

func TestMapperNormalSegment(t *testing.T) {
	m := mapper()
	media, ok := m.ToMedia(450000 + 40161600)
	if !ok || media != 14400+40161600 {
		t.Fatalf("media = %d ok=%v", media, ok)
	}
	pres, ok := m.ToPresentation(14400 + 40161600)
	if !ok || pres != 450000+40161600 {
		t.Fatalf("pres = %d ok=%v", pres, ok)
	}
}

func TestMapperRateHalf(t *testing.T) {
	m := mapper()
	// First presentation tick of segment 3.
	media, ok := m.ToMedia(450000 + 40500000)
	if !ok || media != 40514400 {
		t.Fatalf("media = %d ok=%v", media, ok)
	}
	// Two presentation seconds (180000 ticks) at rate 0.5 -> 90000 media.
	media, ok = m.ToMedia(450000 + 40500000 + 180000)
	if !ok || media != 40514400+90000 {
		t.Fatalf("rate-scaled media = %d ok=%v", media, ok)
	}
}

func TestMapperBeyondEnd(t *testing.T) {
	m := mapper()
	if _, ok := m.ToMedia(m.TotalPres + 1); ok {
		t.Fatal("beyond end must fail")
	}
}

func TestMapperNoElst(t *testing.T) {
	m := NewMapper(nil, 25000, 90000, false)
	media, ok := m.ToMedia(25000) // 1s
	if !ok || media != 90000 {
		t.Fatalf("media = %d ok=%v", media, ok)
	}
}

func TestSnapForward(t *testing.T) {
	m := mapper()
	media, ok := m.SnapForward(225000) // inside empty edit
	if !ok || media != 14400 {
		t.Fatalf("snap media = %d ok=%v", media, ok)
	}
}

func TestMapperDifferingTimescales(t *testing.T) {
	// Movie timescale 25000, track timescale 90000 (wrap fixture):
	// empty 5s, then two 24000s segments at rate 1.
	elst := []moov.ElstEntry{
		{Duration: 125000, MediaTime: -1, Rate: 1 << 16},
		{Duration: 600000000, MediaTime: 0, Rate: 1 << 16},
		{Duration: 600000000, MediaTime: 2160000000, Rate: 1 << 16},
	}
	m := NewMapper(elst, 25000, 90000, false)
	// 13:00:00.000 -> 46800 s -> 1.17e9 movie ticks.
	media, ok := m.ToMedia(1170000000)
	if !ok {
		t.Fatal("target should map")
	}
	// Segment 3 starts at pres 600125000 (24005 s); in-segment delta
	// is 569875000 movie ticks = 22795 s -> media 24000+22795 s.
	want := int64(46795) * 90000
	if media != want {
		t.Fatalf("media = %d, want %d", media, want)
	}
	// Round trip.
	pres, ok := m.ToPresentation(media)
	if !ok || pres != 1170000000 {
		t.Fatalf("pres = %d ok=%v", pres, ok)
	}
}

func TestExpandDTSWrap(t *testing.T) {
	// 1.2M samples * 3600 ticks crosses 2^32 exactly once.
	stts := []moov.SttsEntry{{Count: 1200000, Delta: 3600}}
	dts, wraps, err := ExpandDTS(stts, 1200000)
	if err != nil {
		t.Fatal(err)
	}
	if wraps != 1 {
		t.Fatalf("wraps = %d, want 1", wraps)
	}
	last := dts[len(dts)-1]
	if last != 1199999*3600 {
		t.Fatalf("last dts = %d", last)
	}
	// Monotonic across the wrap boundary.
	for i := 1; i < len(dts); i++ {
		if dts[i] <= dts[i-1] {
			t.Fatalf("dts not monotonic at %d", i)
		}
	}
}

func TestExpandDTSCountMismatch(t *testing.T) {
	stts := []moov.SttsEntry{{Count: 5, Delta: 100}}
	if _, _, err := ExpandDTS(stts, 6); err == nil {
		t.Fatal("expected count mismatch error")
	}
}

func TestComputePTSBFrames(t *testing.T) {
	dts := []int64{0, 3600, 7200, 10800}
	ctts := []moov.CttsEntry{
		{Count: 1, Offset: 7200},  // I displayed late
		{Count: 1, Offset: -1800}, // B
		{Count: 1, Offset: -1800}, // B
		{Count: 1, Offset: 3600},  // P
	}
	pts, err := ComputePTS(dts, ctts)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{7200, 1800, 5400, 14400}
	for i := range want {
		if pts[i] != want[i] {
			t.Fatalf("pts = %v, want %v", pts, want)
		}
	}
	order := PresentationOrder(pts)
	if order[0] != 1 || order[1] != 2 || order[2] != 0 || order[3] != 3 {
		t.Fatalf("presentation order = %v", order)
	}
}

func TestComputePTSNoCtts(t *testing.T) {
	dts := []int64{0, 100, 200}
	pts, err := ComputePTS(dts, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range dts {
		if pts[i] != dts[i] {
			t.Fatalf("pts = %v", pts)
		}
	}
}
