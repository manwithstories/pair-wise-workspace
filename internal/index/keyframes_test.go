package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

func sampleTrack() parse.Track {
	return parse.Track{
		ID:        1,
		Handler:   "vide",
		Timescale: 90000,
		Samples: []parse.Sample{
			{Index: 1, DTS: 0, Duration: 3000, Offset: 1000, Size: 10, Sync: true},
			{Index: 2, DTS: 3000, Duration: 3000, Offset: 1010, Size: 11, Sync: false},
			{Index: 3, DTS: 6000, Duration: 3000, Offset: 1021, Size: 12, Sync: false},
			{Index: 4, DTS: 9000, Duration: 3000, Offset: 1033, Size: 13, Sync: true},
			{Index: 5, DTS: 12000, Duration: 3000, Offset: 1046, Size: 14, Sync: false},
			{Index: 6, DTS: 15000, Duration: 3000, Offset: 1060, Size: 15, Sync: true},
		},
		Keyframes: []uint32{1, 4, 6},
	}
}

func mustBuild(t *testing.T) (*Index, SampleLookup) {
	t.Helper()
	track := sampleTrack()
	idx, err := Build(track)
	if err != nil {
		t.Fatal(err)
	}
	return idx, LookupFromTrack(track)
}

func TestBuildAndValidate(t *testing.T) {
	idx, _ := mustBuild(t)
	if len(idx.Entries) != 3 {
		t.Fatalf("entries %d", len(idx.Entries))
	}
	if idx.Entries[1].Number != 4 || idx.Entries[1].Start != 9000 {
		t.Fatalf("entry: %+v", idx.Entries[1])
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBuildRejectsZeroTimescale(t *testing.T) {
	_, err := Build(parse.Track{ID: 1, Timescale: 0})
	if !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("zero timescale: %v", err)
	}
}

func TestAtVerifiesAgainstTable(t *testing.T) {
	idx, lookup := mustBuild(t)
	cases := []struct {
		tick clock.Tick
		want int
	}{
		{0, 1}, {2999, 1}, {3000, 1}, {8999, 1}, {9000, 4}, {14999, 4}, {15000, 6}, {1 << 40, 6},
	}
	for _, c := range cases {
		e, err := idx.At(c.tick, lookup)
		if err != nil {
			t.Fatalf("at %d: %v", c.tick, err)
		}
		if e.Number != c.want {
			t.Fatalf("at %d: got sample %d want %d", c.tick, e.Number, c.want)
		}
	}
}

func TestAtBeforeFirstKeyframeIsGap(t *testing.T) {
	idx, lookup := mustBuild(t)
	idx.Entries[0].Start = 1000
	idx.Entries[0].Number = 1
	if _, err := idx.At(0, lookup); !errors.Is(err, model.ErrGap) {
		t.Fatalf("before first: %v", err)
	}
}

func TestCorruptIndexDetected(t *testing.T) {
	idx, lookup := mustBuild(t)
	idx.Entries[1].Start += 1 // stale entry
	if _, err := idx.At(9500, lookup); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("stale entry: %v", err)
	}
	// Unsorted index is rejected outright.
	idx2, _ := mustBuild(t)
	idx2.Entries[0], idx2.Entries[2] = idx2.Entries[2], idx2.Entries[0]
	if err := idx2.Validate(); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("unsorted: %v", err)
	}
}

func TestBatchRebuildsOnce(t *testing.T) {
	idx, lookup := mustBuild(t)
	idx.Entries[0].ByteLen = 999 // corrupt but structurally fine
	rebuilds := 0
	queries := GenerateQueries(idx)
	fresh, cov, err := Batch(idx, queries, lookup, func() (*Index, error) {
		rebuilds++
		return Build(sampleTrack())
	})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilds != 1 {
		t.Fatalf("rebuilds %d", rebuilds)
	}
	if !cov.Rebuilt || cov.Hits != len(queries) {
		t.Fatalf("coverage %+v of %d", cov, len(queries))
	}
	if fresh.Entries[0].ByteLen != 10 {
		t.Fatalf("rebuilt entry: %+v", fresh.Entries[0])
	}
	if cov.Rate() != 1 {
		t.Fatalf("rate %f", cov.Rate())
	}
}

func TestBatchWithoutRebuildFails(t *testing.T) {
	idx, lookup := mustBuild(t)
	idx.Entries[0].ByteOff = -1
	if _, _, err := Batch(idx, []clock.Tick{0}, lookup, nil); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("no rebuild: %v", err)
	}
}

func TestGenerateQueries(t *testing.T) {
	idx, _ := mustBuild(t)
	q := GenerateQueries(idx)
	// starts 0, 9000, 15000 plus midpoints 4500 and 12000
	if len(q) != 5 {
		t.Fatalf("queries: %v", q)
	}
	if q[1] != 4500 || q[3] != 12000 {
		t.Fatalf("midpoints: %v", q)
	}
	if (Coverage{}).Rate() != 0 {
		t.Fatal("empty coverage rate")
	}
}

func TestSaveLoad(t *testing.T) {
	idx, _ := mustBuild(t)
	path := filepath.Join(t.TempDir(), "index.json")
	if err := idx.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != len(idx.Entries) || back.Entries[2].Start != idx.Entries[2].Start {
		t.Fatalf("loaded: %+v", back.Entries)
	}
	// Corrupt file must be reported, not silently accepted.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("corrupt file: %v", err)
	}
	bad := *idx
	bad.Version = 99
	if err := bad.Save(path); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("version: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file loaded")
	}
}
