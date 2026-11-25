package index

import (
	"testing"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// buildTrack makes a track whose sample offsets index into a synthetic buffer.
func buildTrack(n int, dataLen int64) (*model.Track, []byte) {
	tr := &model.Track{
		Number: 0, ID: 4, Kind: model.Video, Codec: model.CodecMJPEG,
		Timebase: model.MustTimebase(90000, 1),
	}
	data := make([]byte, dataLen)
	var off int64
	for i := 0; i < n; i++ {
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, TrackID: 4, Offset: off, Size: 10,
			DTS: int64(i) * 3000, Duration: 3000, Sync: i%4 == 0,
		})
		off += 10
	}
	return tr, data
}

func TestBuildIndexesSyncSamples(t *testing.T) {
	tr, data := buildTrack(20, 200)
	idx := Build([]*model.Track{tr}, data)
	if !idx.Valid {
		t.Fatalf("valid index flagged invalid: %v", idx.Faults)
	}
	if idx.Count() != 5 {
		t.Fatalf("entries = %d, want 5", idx.Count())
	}
	for _, e := range idx.Entries {
		if e.TrackID != 4 {
			t.Fatalf("entry track = %d", e.TrackID)
		}
		if e.Offset < 0 || e.Offset+e.Size > int64(len(data)) {
			t.Fatalf("entry outside buffer: %+v", e)
		}
	}
}

func TestValidateRejectsMismatchedOffset(t *testing.T) {
	tr, data := buildTrack(8, 80)
	idx := &Index{Faults: &fault.List{}}
	for _, s := range tr.Samples {
		if s.Sync {
			idx.Entries = append(idx.Entries, Entry{
				TrackID: 4, SampleIdx: s.Index, Offset: s.Offset + 1, Size: s.Size,
			})
		}
	}
	if err := idx.Validate([]*model.Track{tr}, data); err == nil {
		t.Fatal("offset mismatch was accepted")
	}
}

func TestRevalidateRebuildsAfterCorruption(t *testing.T) {
	tr, data := buildTrack(12, 120)
	bad := &Index{Faults: &fault.List{}, Entries: []Entry{
		{TrackID: 99, SampleIdx: 0, Offset: 0, Size: 10},
	}}
	if err := bad.Validate([]*model.Track{tr}, data); err == nil {
		t.Fatal("unknown track was accepted")
	}
	bad.Revalidate([]*model.Track{tr}, data)
	if !bad.Valid {
		t.Fatalf("rebuild left the index invalid: %v", bad.Faults)
	}
	if bad.Rebuilds != 1 {
		t.Fatalf("rebuilds = %d, want 1", bad.Rebuilds)
	}
	want := 0
	for _, s := range tr.Samples {
		if s.Sync {
			want++
		}
	}
	if bad.Count() != want {
		t.Fatalf("rebuilt entries = %d, want %d", bad.Count(), want)
	}
}

func TestValidateRejectsOutOfRangeOffset(t *testing.T) {
	tr, data := buildTrack(4, 40)
	idx := &Index{Faults: &fault.List{}, Entries: []Entry{
		{TrackID: 4, SampleIdx: 0, Offset: 0, Size: dataLenPlus(data, 1)},
	}}
	if err := idx.Validate([]*model.Track{tr}, data); err == nil {
		t.Fatal("out-of-range entry was accepted")
	}
}

func dataLenPlus(data []byte, n int64) int64 { return int64(len(data)) + n }

func TestShardIsDeterministic(t *testing.T) {
	tr, data := buildTrack(40, 400)
	idx := Build([]*model.Track{tr}, data)
	a := idx.Shard(5)
	b := idx.Shard(5)
	if len(a) != len(b) {
		t.Fatalf("shard counts differ")
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			t.Fatalf("shard %d sizes differ", i)
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("shard %d entry %d differs", i, j)
			}
		}
	}
	total := 0
	for _, s := range a {
		total += len(s)
	}
	if total != idx.Count() {
		t.Fatalf("shards cover %d of %d entries", total, idx.Count())
	}
}
