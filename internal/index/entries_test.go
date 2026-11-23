package index

import (
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/parse"
	"archivecheck/internal/repair"
	"archivecheck/internal/testkit"
)

func buildTimeline(t *testing.T) (*repair.Timeline, *testkit.Manifest) {
	t.Helper()
	initSpec, fragSpecs := testkit.StandardSampleLayout()
	m, err := testkit.Generate(t.TempDir(), initSpec, fragSpecs)
	if err != nil {
		t.Fatal(err)
	}
	initFile, err := os.Open(m.Segments[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer initFile.Close()
	init, err := parse.ParseInit(initFile, "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var frags []*parse.Fragment
	for _, s := range m.Segments[1:] {
		f, err := os.Open(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		frag, err := parse.ParseFragment(f, s.Name, init)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, frag)
	}
	ord, err := repair.Order(frags)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := repair.Build(ord, init)
	if err != nil {
		t.Fatal(err)
	}
	return tl, m
}

func TestBuildIndex(t *testing.T) {
	tl, _ := buildTimeline(t)
	idx, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 48 {
		t.Fatalf("entries = %d, want 48", len(idx.Entries))
	}
	if idx.Unseekable != 0 {
		t.Fatalf("unseekable = %d", idx.Unseekable)
	}
	if len(idx.Intervals) != 12 {
		t.Fatalf("intervals = %d, want 12", len(idx.Intervals))
	}
	if len(idx.SeekPoints()) != 12 {
		t.Fatalf("seek points = %d", len(idx.SeekPoints()))
	}
	last := map[uint32]uint64{}
	for i, e := range idx.Entries {
		if e.DTS < last[e.TrackID] {
			t.Fatalf("entry %d regresses", i)
		}
		last[e.TrackID] = e.EndDTS
	}
}

func TestIndexMarksUnseekableInterval(t *testing.T) {
	init := &parse.Init{Segment: "init.mp4", Tracks: map[uint32]*parse.Track{
		1: {ID: 1, Timescale: 1000},
	}}
	ord := &repair.Ordered{Fragments: []*parse.Fragment{{
		Segment: "a", Sequence: 1, HasMfhd: true,
		Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: 0, Samples: []parse.Sample{
			{TrackID: 1, Duration: 100, Size: 10, Flags: 0x01010000, Offset: 100},
			{TrackID: 1, Duration: 100, Size: 10, Flags: 0x01010000, Offset: 110},
		}}},
	}}}
	tl, err := repair.Build(ord, init)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Unseekable != 1 || idx.Intervals[0].Seekable {
		t.Fatalf("unseekable = %d intervals = %+v", idx.Unseekable, idx.Intervals)
	}
	if len(idx.SeekPoints()) != 0 {
		t.Fatalf("seek points = %d, want 0", len(idx.SeekPoints()))
	}
}

func TestValidateRejectsBadRange(t *testing.T) {
	idx := &Index{Entries: []Entry{{TrackID: 1, DTS: 0, EndDTS: 10, Offset: 100, Size: 50}}}
	if err := idx.Validate(120); err == nil {
		t.Fatal("expected out-of-file sample range to fail")
	}
	if err := idx.Validate(1000); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRoundTripAndTamper(t *testing.T) {
	tl, _ := buildTimeline(t)
	idx, err := Build(tl)
	if err != nil {
		t.Fatal(err)
	}
	cp := &Checkpoint{
		Version: CheckpointVersion, InputDigest: "abc", CommittedDigest: "def",
		CommittedFragments: []string{"frag-001.m4s"}, LastCommitted: "frag-001.m4s",
		FragmentsTotal: 12, RepairedTfdt: 2, MissingMfhd: 1, Index: idx,
	}
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := Save(path, cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Checksum == "" || loaded.InputDigest != "abc" || len(loaded.Index.Entries) != 48 {
		t.Fatalf("loaded = %+v", loaded)
	}
	raw, _ := os.ReadFile(path)
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	for i := range tampered {
		if bytesContains(tampered[i:], []byte(`"abc"`)) {
			tampered[i] = 'x'
			break
		}
	}
	if _, err := Decode(tampered); err == nil {
		t.Fatal("tampered checkpoint accepted")
	}
}

func bytesContains(h, n []byte) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if h[i+j] != n[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
