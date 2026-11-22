package index

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
)

func sampleEntries() []Entry {
	return []Entry{
		{TrackID: 0, Sequence: 0, Start: 0, End: clock.Ticks(4 * clock.Rate), Size: 1000},
		{TrackID: 0, Sequence: 1, Start: clock.Ticks(4 * clock.Rate), End: clock.Ticks(8 * clock.Rate), Size: 1200},
		{TrackID: 1, Sequence: 0, Start: 0, End: clock.Ticks(4 * clock.Rate), Size: 800},
		{TrackID: 1, Sequence: 1, Start: clock.Ticks(4 * clock.Rate), End: clock.Ticks(8 * clock.Rate), Size: 900},
	}
}

func TestBuilderCanonicalOrderAndOffsets(t *testing.T) {
	// Add in shuffled order; Build must canonicalize.
	shuffled := []Entry{sampleEntries()[3], sampleEntries()[0], sampleEntries()[2], sampleEntries()[1]}
	b := NewBuilder()
	for _, e := range shuffled {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 4 || idx.Timescale != clock.Rate {
		t.Fatalf("index wrong: %+v", idx)
	}
	// Canonical order: (Start, TrackID, Sequence).
	t4 := clock.Ticks(4 * clock.Rate)
	want := [][2]any{{clock.Ticks(0), 0}, {clock.Ticks(0), 1}, {t4, 0}, {t4, 1}}
	for i, w := range want {
		got := [2]any{idx.Entries[i].Start, idx.Entries[i].TrackID}
		if got != w {
			t.Fatalf("entry %d = %v, want %v", i, got, w)
		}
	}
	// Offsets regenerated contiguously.
	wantOff := []int64{0, 1000, 1800, 3000}
	for i, e := range idx.Entries {
		if e.Offset != wantOff[i] {
			t.Fatalf("offset %d = %d, want %d", i, e.Offset, wantOff[i])
		}
	}
}

func TestDigestDeterministic(t *testing.T) {
	d1, err := RerunDigest(sampleEntries())
	if err != nil {
		t.Fatal(err)
	}
	// Shuffled input must produce the identical digest.
	shuffled := []Entry{sampleEntries()[2], sampleEntries()[3], sampleEntries()[0], sampleEntries()[1]}
	d2, err := RerunDigest(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || len(d1) != 64 {
		t.Fatalf("digest mismatch or bad length: %s vs %s", d1, d2)
	}
	// Untrusted flag must change the digest (it is part of encoding).
	flagged := sampleEntries()
	flagged[0].Untrusted = true
	d3, err := RerunDigest(flagged)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatal("untrusted flag must participate in digest")
	}
}

func TestBuilderRejectsBadEntries(t *testing.T) {
	b := NewBuilder()
	if err := b.Add(Entry{Start: 100, End: 50, Size: 10}); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("inverted range: %v", err)
	}
	e := sampleEntries()[0]
	if err := b.Add(e); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(e); !errors.Is(err, clock.ErrDuplicateSeq) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestVerifyRejectsOverlapAndGaps(t *testing.T) {
	b := NewBuilder()
	good := sampleEntries()
	for _, e := range good {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	// Tamper an offset to break contiguity.
	tampered := *idx
	tampered.Entries = append([]Entry(nil), idx.Entries...)
	tampered.Entries[2].Offset += 5
	if err := Verify(&tampered); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("offset gap: %v", err)
	}
}

func TestSeekAtAndMisses(t *testing.T) {
	b := NewBuilder()
	for _, e := range sampleEntries() {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, _ := b.Build()
	table, err := NewSeekTable(idx)
	if err != nil {
		t.Fatal(err)
	}
	// Seek before the first entry -> miss.
	if r := table.SeekAt(clock.Ticks(-1), -1); !r.Miss || r.Found {
		t.Fatalf("negative seek should miss: %+v", r)
	}
	// Seek at 1s on track 0 returns the t=0 entry (latest <= t).
	r := table.SeekAt(clock.Ticks(clock.Rate), 0)
	if !r.Found || r.Entry.TrackID != 0 || r.Entry.Start != clock.Ticks(0) {
		t.Fatalf("seek 1s track0 wrong: %+v", r)
	}
	// Exact match at 4s.
	r = table.SeekExact(clock.Ticks(4*clock.Rate), 1)
	if !r.Found || r.Entry.Sequence != 1 || r.Entry.TrackID != 1 {
		t.Fatalf("exact seek wrong: %+v", r)
	}
	// Exact miss between keyframes.
	r = table.SeekExact(clock.Ticks(clock.Rate), 0)
	if !r.Miss {
		t.Fatalf("non-keyframe exact seek should miss: %+v", r)
	}
	lookups, misses := table.Stats()
	if lookups != 4 || misses != 2 {
		t.Fatalf("stats lookups=%d misses=%d", lookups, misses)
	}
}

func TestSeekUntrustedDegraded(t *testing.T) {
	entries := sampleEntries()
	entries[1].Untrusted = true
	b := NewBuilder()
	for _, e := range entries {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, _ := b.Build()
	table, _ := NewSeekTable(idx)
	r := table.SeekAt(clock.Ticks(5*clock.Rate), 0)
	if !r.Found || !r.Degraded || !r.Entry.Untrusted {
		t.Fatalf("untrusted seek should be degraded: %+v", r)
	}
}

func TestQuarantineIsolation(t *testing.T) {
	b := NewBuilder()
	for _, e := range sampleEntries() {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, _ := b.Build()
	cleanDigest, _ := idx.Digest()
	table, _ := NewSeekTable(idx)
	// A corrupt fragment is quarantined, never indexed.
	table.Quarantine(0, 99, "truncated moof")
	table.Quarantine(1, 7, "bad trun")
	q := table.QuarantineList()
	if len(q) != 2 || q[0].Reason != "truncated moof" {
		t.Fatalf("quarantine wrong: %+v", q)
	}
	// Index and digest remain untouched by quarantine records.
	if d, _ := table.Index().Digest(); d != cleanDigest {
		t.Fatal("quarantine mutated the index digest")
	}
}

func TestMarshalSIDXRoundTripShape(t *testing.T) {
	b := NewBuilder()
	for _, e := range sampleEntries() {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	idx, _ := b.Build()
	raw, err := idx.MarshalSIDX()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[4:8]) != "sidx" {
		t.Fatal("missing sidx box type")
	}
	// Deterministic byte output.
	raw2, _ := idx.MarshalSIDX()
	if len(raw) != len(raw2) {
		t.Fatal("sidx length differs across runs")
	}
	for i := range raw {
		if raw[i] != raw2[i] {
			t.Fatal("sidx serialization non-deterministic")
		}
	}
}
