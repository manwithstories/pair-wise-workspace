package clock

import (
	"testing"
)

func ms(v int64) int64 { return v * 1_000_000 }

func segID(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return "seg-0" + string(digits[i])
	}
	return "seg-" + string(digits[i/10]) + string(digits[i%10])
}

func base96(n int) []Observed {
	out := make([]Observed, n)
	for i := range out {
		out[i] = Observed{
			ID:       segID(i),
			Start:    uint64(i * 90000 * 2),
			HasStart: true,
			Duration: ms(2000),
			Valid:    true,
		}
	}
	return out
}

func TestToNanosRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		units     uint64
		timescale uint32
		want      int64
	}{
		{0, 90000, 0},
		{90000, 90000, 1_000_000_000},
		{45000, 90000, 500_000_000},
		{48000, 48000, 1_000_000_000},
		{44100, 44100, 1_000_000_000},
		{1, 90000, 11111},
	} {
		if got := ToNanos(tc.units, tc.timescale); got != tc.want {
			t.Errorf("ToNanos(%d,%d)=%d want %d", tc.units, tc.timescale, got, tc.want)
		}
	}
	if got := FromNanos(1_000_000_000, 48000); got != 48000 {
		t.Errorf("FromNanos(1s,48000)=%d want 48000", got)
	}
	if got := NanosDelta(-90000, 90000); got != -1_000_000_000 {
		t.Errorf("NanosDelta(-90000)=%d", got)
	}
}

func TestCleanTimelineIsStable(t *testing.T) {
	m := Build(base96(100), Options{})
	if len(m.Unavailable) != 0 {
		t.Fatalf("unexpected unavailable: %v", m.Unavailable)
	}
	if got := m.Entries[0].Start; got != 0 {
		t.Errorf("first start %d", got)
	}
	if got := m.Entries[99].Start; got != ms(2000*99) {
		t.Errorf("last start %d want %d", got, ms(2000*99))
	}
	for _, e := range m.Entries {
		if e.Action != ActionOK {
			t.Fatalf("segment %s action %s, want ok", e.ID, e.Action)
		}
	}
}

func TestWraparoundExpanded(t *testing.T) {
	obs := base96(20)
	const mod = 1 << 33
	const shift = uint64(mod - 90000*20)
	for i := range obs {
		obs[i].Start = (uint64(i*90000*2) + shift) % mod
	}
	m := Build(obs, Options{})
	if len(m.Unavailable) != 0 {
		t.Fatalf("unavailable: %v", m.Unavailable)
	}
	var sawWrap bool
	for i, e := range m.Entries {
		if e.Action == ActionUnwrapped {
			sawWrap = true
		}
		// The mapping is relative to the first timestamp, so segment i starts
		// at i*2s regardless of where the raw counter sat.
		want := int64(i) * ms(2000)
		if e.Start != want {
			t.Fatalf("entry %d start %d want %d action %s", i, e.Start, want, e.Action)
		}
	}
	if !sawWrap {
		t.Fatal("expected an unwrapped action across the wrap")
	}
	if len(m.Warnings) == 0 {
		t.Error("expected a wraparound warning")
	}
}

func TestRepairableBackwardJump(t *testing.T) {
	obs := base96(10)
	obs[5].Start -= 90000 // one second backwards
	m := Build(obs, Options{})
	if len(m.Unavailable) != 0 {
		t.Fatalf("unavailable: %v", m.Unavailable)
	}
	if m.Entries[5].Action != ActionShifted {
		t.Fatalf("action=%s want shifted", m.Entries[5].Action)
	}
	if m.Entries[5].Start != ms(2000*5) {
		t.Fatalf("start=%d", m.Entries[5].Start)
	}
	if m.Entries[6].Start != ms(2000*6) {
		t.Fatalf("entry 6 start=%d", m.Entries[6].Start)
	}
}

func TestUnrepairableBackwardJumpQuarantined(t *testing.T) {
	obs := base96(10)
	obs[5].Start = 0
	m := Build(obs, Options{})
	if len(m.Unavailable) != 1 || m.Unavailable[0] != "seg-05" {
		t.Fatalf("unavailable=%v", m.Unavailable)
	}
	if m.Entries[5].Available {
		t.Fatal("entry 5 must be unavailable")
	}
	if m.Entries[6].Start != ms(2000*6) {
		t.Fatalf("a quarantined segment must not drag the grid: %d", m.Entries[6].Start)
	}
}

func TestDuplicateReSlotted(t *testing.T) {
	obs := base96(10)
	obs[4].Start = obs[3].Start
	m := Build(obs, Options{})
	if m.Entries[4].Action != ActionDuplicate {
		t.Fatalf("action=%s want duplicate", m.Entries[4].Action)
	}
	if m.Entries[4].Start != ms(2000*4) {
		t.Fatalf("start=%d want %d", m.Entries[4].Start, ms(2000*4))
	}
}

func TestMissingTimestampInferredOnlyWithBase(t *testing.T) {
	obs := base96(6)
	obs[2].HasStart = false
	m := Build(obs, Options{})
	if len(m.Unavailable) != 0 {
		t.Fatalf("middle hole should be inferred: %v", m.Unavailable)
	}
	if m.Entries[2].Action != ActionGap {
		t.Fatalf("action=%s", m.Entries[2].Action)
	}
	if m.Entries[2].Start != ms(4000) {
		t.Fatalf("start=%d want 4000ms", m.Entries[2].Start)
	}

	first := base96(6)
	first[0].HasStart = false
	m2 := Build(first, Options{})
	if len(m2.Unavailable) != 1 || m2.Unavailable[0] != "seg-00" {
		t.Fatalf("a missing leading timestamp has no base: %v", m2.Unavailable)
	}
}

func TestConsecutiveHolesQuarantined(t *testing.T) {
	obs := base96(8)
	obs[3].HasStart = false
	obs[4].HasStart = false
	m := Build(obs, Options{})
	if len(m.Unavailable) != 2 {
		t.Fatalf("a run of two holes is ambiguous and must be quarantined: %v", m.Unavailable)
	}
	for _, id := range []string{"seg-03", "seg-04"} {
		e, ok := m.Lookup(id)
		if !ok || e.Available {
			t.Fatalf("%s should be unavailable", id)
		}
	}
	// The grid must recover for segments after the hole.
	if m.Entries[5].Start != ms(2000*5) || m.Entries[5].Action != ActionOK {
		t.Fatalf("post-hole grid: %+v", m.Entries[5])
	}
}

func TestTrailingHoleQuarantined(t *testing.T) {
	obs := base96(5)
	obs[4].HasStart = false
	m := Build(obs, Options{})
	if len(m.Unavailable) != 1 || m.Unavailable[0] != "seg-04" {
		t.Fatalf("a hole with no following evidence is not uniquely inferable: %v", m.Unavailable)
	}
}

func TestCorruptHeaderUnavailable(t *testing.T) {
	obs := base96(5)
	obs[2].Valid = false
	m := Build(obs, Options{})
	if len(m.Unavailable) != 1 || m.Unavailable[0] != "seg-02" {
		t.Fatalf("unavailable=%v", m.Unavailable)
	}
	if m.Entries[2].Action != ActionUnavailable {
		t.Fatalf("action=%s", m.Entries[2].Action)
	}
}

func TestDeterministicDigest(t *testing.T) {
	obs := base96(40)
	obs[7].Start += 3
	obs[12].Start = 0
	obs[20].HasStart = false
	obs[30].Start = (1 << 33) - 1

	a := Build(obs, Options{})
	b := Build(obs, Options{})
	if a.Digest != b.Digest {
		t.Fatalf("digest not stable: %s vs %s", a.Digest, b.Digest)
	}
	if len(a.Digest) != 64 {
		t.Fatalf("digest length %d", len(a.Digest))
	}
	rev := append([]Repair(nil), a.Entries...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if DigestEntries(rev) != a.Digest {
		t.Fatal("DigestEntries must be order independent")
	}
}

func TestLookup(t *testing.T) {
	m := Build(base96(3), Options{})
	e, ok := m.Lookup("seg-02")
	if !ok || e.Start != ms(4000) {
		t.Fatalf("lookup=%v ok=%v", e, ok)
	}
	if _, ok := m.Lookup("nope"); ok {
		t.Fatal("unexpected lookup hit")
	}
}
