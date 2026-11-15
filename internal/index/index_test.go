package index

import (
	"bytes"
	"errors"
	"testing"

	"mp4probe/internal/box"
	"mp4probe/internal/moov"
)

// fakeMoov builds a two-track moov with 10 video samples in 2 chunks
// and 4 audio samples in 1 chunk, all inside a 4000-byte mdat.
func fakeMoov() *moov.Moov {
	video := &moov.Trak{
		TrackID: 1, Timescale: 90000, MediaDuration: 36000, TkhdDuration: 36000,
		Handler: "vide", Codec: "avc1",
		Stts:        []moov.SttsEntry{{Count: 10, Delta: 3600}},
		Stss:        []uint32{1, 6},
		HasStss:     true,
		Stsc:        []moov.StscEntry{{FirstChunk: 1, SamplesPerChunk: 5, SampleDescIndex: 1}},
		Sizes:       []uint32{100, 100, 100, 100, 100, 100, 100, 100, 100, 100},
		ChunkOffset: []uint64{100, 600},
	}
	audio := &moov.Trak{
		TrackID: 2, Timescale: 48000, MediaDuration: 4096, TkhdDuration: 36000,
		Handler: "soun", Codec: "mp4a",
		Stts:        []moov.SttsEntry{{Count: 4, Delta: 1024}},
		Stsc:        []moov.StscEntry{{FirstChunk: 1, SamplesPerChunk: 4, SampleDescIndex: 1}},
		Sizes:       []uint32{20, 20, 20, 20},
		ChunkOffset: []uint64{1100},
	}
	return &moov.Moov{
		MovieTimescale: 90000, MovieDuration: 36000,
		Traks:      []*moov.Trak{video, audio},
		MdatOffset: 100, MdatEnd: 4100, HasMdat: true,
	}
}

func TestBuildAndValidate(t *testing.T) {
	idx, err := Build("fake.mp4", 8192, fakeMoov())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Tracks) != 2 || idx.Tracks[0].TrackID != 1 {
		t.Fatalf("tracks not ordered by id: %+v", idx.Tracks)
	}
	v := idx.Tracks[0]
	if v.SampleCount != 10 || v.SyncSampleCount != 2 || v.ChunkCount != 2 {
		t.Fatalf("bad counts: %+v", v)
	}
	// Second chunk starts at 600; sample 5 offset must be 600.
	if v.Offsets[5] != 600 || v.Offsets[4] != 500 {
		t.Fatalf("bad offsets: %v", v.Offsets)
	}
	if v.Sync[0] != 0 || v.Sync[1] != 5 {
		t.Fatalf("sync not 0-based: %v", v.Sync)
	}
	a := idx.Tracks[1]
	if !a.AllSync || a.SyncSampleCount != 4 {
		t.Fatalf("audio should be all-sync: %+v", a)
	}
	if err := idx.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBuildStscStszMismatch(t *testing.T) {
	m := fakeMoov()
	m.Traks[0].Stsc[0].SamplesPerChunk = 4 // 2 chunks * 4 = 8 < 10 samples
	if _, err := Build("fake.mp4", 8192, m); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestBuildChunkOffsetOutOfBounds(t *testing.T) {
	m := fakeMoov()
	m.Traks[0].ChunkOffset[1] = 99999
	if _, err := Build("fake.mp4", 8192, m); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestBuildSttsStszMismatch(t *testing.T) {
	m := fakeMoov()
	m.Traks[0].Stts[0].Count = 11
	if _, err := Build("fake.mp4", 8192, m); !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	idx, err := Build("fake.mp4", 8192, fakeMoov())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Save(idx, &buf); err != nil {
		t.Fatal(err)
	}
	back, err := Load(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	if back.Tracks[0].Offsets[5] != 600 || back.Tracks[1].Sizes[3] != 20 {
		t.Fatalf("roundtrip mismatch: %+v", back.Tracks[0])
	}
}

func TestValidateDetectsCorruption(t *testing.T) {
	idx, err := Build("fake.mp4", 8192, fakeMoov())
	if err != nil {
		t.Fatal(err)
	}
	cases := []func(*Index){
		func(i *Index) { i.Tracks[0].DTS[3] = -1 },               // dts regression
		func(i *Index) { i.Tracks[0].Sync[1] = 99 },              // sync out of range
		func(i *Index) { i.Tracks[0].Offsets[2] = 1 },            // before mdat
		func(i *Index) { i.Tracks[0].Sizes[2] = 99999 },          // past mdat end
		func(i *Index) { i.Tracks[0].PTS = i.Tracks[0].PTS[:5] }, // length mismatch
		func(i *Index) { i.Version = 99 },                        // bad version
		func(i *Index) { i.MdatEnd = i.Size + 1 },                // mdat past EOF
	}
	for n, corrupt := range cases {
		clone, err := Build("fake.mp4", 8192, fakeMoov())
		if err != nil {
			t.Fatal(err)
		}
		corrupt(clone)
		if err := clone.Validate(); !errors.Is(err, ErrInconsistent) {
			t.Fatalf("case %d: want inconsistency, got %v", n, err)
		}
	}
	_ = idx
}
