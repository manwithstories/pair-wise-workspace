package export

import (
	"bytes"
	"errors"
	"testing"

	"mp4probe/internal/box"
	"mp4probe/internal/index"
	"mp4probe/internal/moov"
	"mp4probe/internal/timeline"
)

// setup builds a 100-sample track (delta 3600, sync every 20) whose
// sample i is the 4 bytes [i, i, i, i], plus an edit list with an
// empty edit and a rate-0.5 tail.
func setup() (*index.Index, *index.TrackIndex, []byte, *timeline.Mapper) {
	n := 100
	tr := &index.TrackIndex{
		TrackID: 1, Timescale: 90000, MediaDuration: 360000,
		SampleCount: n, SyncSampleCount: 5, ChunkCount: 5,
	}
	var data []byte
	for i := 0; i < n; i++ {
		tr.Offsets = append(tr.Offsets, int64(len(data)))
		tr.Sizes = append(tr.Sizes, 4)
		tr.DTS = append(tr.DTS, int64(i)*3600)
		tr.PTS = append(tr.PTS, int64(i)*3600)
		data = append(data, byte(i), byte(i), byte(i), byte(i))
	}
	for i := 0; i < n; i += 20 {
		tr.Sync = append(tr.Sync, uint32(i))
	}
	idx := &index.Index{
		File: "t.mp4", MdatOffset: 0, MdatEnd: uint64(len(data)),
		Tracks: []*index.TrackIndex{tr},
	}
	elst := []moov.ElstEntry{
		{Duration: 90000, MediaTime: -1, Rate: 1 << 16},      // 1s dwell
		{Duration: 180000, MediaTime: 0, Rate: 1 << 16},      // media 0..180000
		{Duration: 360000, MediaTime: 180000, Rate: 1 << 15}, // media 180000..360000 at 0.5x
	}
	mapper := timeline.NewMapper(elst, 90000, 90000, false)
	return idx, tr, data, mapper
}

func TestRangeAcrossElstSegments(t *testing.T) {
	idx, _, data, mapper := setup()
	// Range covering the end of segment 2 and start of segment 3:
	// pres [90000+170000, 90000+190000) -> media 170000..180000 (rate 1)
	// and media 180000..190000 (rate 0.5).
	m, err := Range(idx, 1, mapper, bytes.NewReader(data), 260000, 280000, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if m.Count == 0 {
		t.Fatal("empty manifest")
	}
	// Presentation pts must be continuous and sorted.
	for i := 1; i < len(m.Samples); i++ {
		if m.Samples[i].PTS <= m.Samples[i-1].PTS {
			t.Fatalf("not sorted at %d: %v", i, m.Samples)
		}
	}
	// First sample: pres >= 260000 -> media >= 170000 -> sample 48.
	if m.Samples[0].MediaPTS != 172800 || m.Samples[0].SampleIndex != 48 {
		t.Fatalf("first sample %+v", m.Samples[0])
	}
	// Verify passes on unmodified data.
	res, err := Verify(bytes.NewReader(data), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatches != 0 || res.CheckedSamples != m.Count {
		t.Fatalf("verify: %+v", res)
	}
}

func TestRangeBeyondMdatFails(t *testing.T) {
	idx, tr, data, mapper := setup()
	tr.Sizes[50] = 99999 // sample 50 now extends past mdat end
	// Range covering sample 50: pres = 90000 + 50*3600 = 270000.
	_, err := Range(idx, 1, mapper, bytes.NewReader(data), 269000, 271000, "a", "b")
	if !errors.Is(err, box.ErrStructure) {
		t.Fatalf("want structural error, got %v", err)
	}
}

func TestVerifyDetectsMismatch(t *testing.T) {
	idx, _, data, mapper := setup()
	m, err := Range(idx, 1, mapper, bytes.NewReader(data), 90000, 90000+72000, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 20 {
		t.Fatalf("count=%d want 20", m.Count)
	}
	// Corrupt one byte of the second sample in the manifest.
	bad := append([]byte(nil), data...)
	bad[m.Samples[1].Offset] ^= 0xFF
	res, err := Verify(bytes.NewReader(bad), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatches != 1 || res.MismatchSamples[0].SampleIndex != m.Samples[1].SampleIndex {
		t.Fatalf("verify: %+v", res)
	}
	if res.MismatchSamples[0].Expected == res.MismatchSamples[0].Actual {
		t.Fatal("expected/actual must differ")
	}
}

func TestRangeInvalid(t *testing.T) {
	idx, _, data, mapper := setup()
	if _, err := Range(idx, 1, mapper, bytes.NewReader(data), 100, 100, "a", "b"); err == nil {
		t.Fatal("expected invalid range error")
	}
}
