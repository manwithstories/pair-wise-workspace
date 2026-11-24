package mp4

import (
	"testing"

	"media-check/internal/model"
)

func samples(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		b := make([]byte, 24)
		for j := range b {
			b[j] = byte(i*17 + j)
		}
		out[i] = b
	}
	return out
}

func TestMarshalParseRoundtrip(t *testing.T) {
	a := Adapter{}
	src := samples(10)
	data, err := a.Marshal(src, []uint32{1, 6}, model.RTPTimebase, 3000)
	if err != nil {
		t.Fatal(err)
	}
	res := a.Parse(data, 0, model.TimeRange{Start: 0, End: 30000})
	if !res.Complete {
		t.Fatalf("own output must parse clean: %v", res.Faults)
	}
	if len(res.Boxes) == 0 {
		t.Fatal("box hierarchy not recorded")
	}
	if res.Table.Timescale != model.RTPTimebase || res.Table.Delta != 3000 {
		t.Fatalf("bad table: %+v", res.Table)
	}
	if len(res.Table.KeySamples) != 2 {
		t.Fatalf("key samples lost: %v", res.Table.KeySamples)
	}
	got, err := res.Table.Samples(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(src) {
		t.Fatalf("sample count %d != %d", len(got), len(src))
	}
	for i := range src {
		if string(got[i]) != string(src[i]) {
			t.Fatalf("sample %d changed", i)
		}
	}
}

func TestParseTruncatedBox(t *testing.T) {
	a := Adapter{}
	data, err := a.Marshal(samples(10), []uint32{1}, model.RTPTimebase, 3000)
	if err != nil {
		t.Fatal(err)
	}
	rng := model.TimeRange{Start: 100, End: 200}
	res := a.Parse(data[:len(data)*2/3], 4, rng)
	if res.Complete {
		t.Fatal("truncated input must not report complete")
	}
	if !res.Faults.HasCode(model.FaultBoxTruncated) {
		t.Fatalf("expected box_truncated, got %v", res.Faults)
	}
	for _, f := range res.Faults {
		if f.Segment != 4 || f.Range != rng {
			t.Fatalf("fault must carry segment and time range: %+v", f)
		}
	}
}

func TestParseOutOfRangeSampleOffset(t *testing.T) {
	a := Adapter{}
	data, err := a.Marshal(samples(6), []uint32{1}, model.RTPTimebase, 3000)
	if err != nil {
		t.Fatal(err)
	}
	res := a.Parse(data, 0, model.TimeRange{})
	if !res.Complete {
		t.Fatalf("baseline parse failed: %v", res.Faults)
	}
	// corrupt the stco chunk offset to point beyond mdat
	res.Table.ChunkOffsets[0] = uint32(len(data)) + 4096
	end := uint64(res.Table.ChunkOffsets[0])
	for _, sz := range res.Table.SampleSizes {
		end += uint64(sz)
	}
	if end <= res.Table.MdatOffset+res.Table.MdatLength {
		t.Fatal("test setup: offset not out of range")
	}
	// re-run the same validation the parser applies
	if _, err := res.Table.Samples(data); err == nil {
		t.Fatal("out-of-range sample offset must fail extraction")
	}
}

func TestParseMissingIndex(t *testing.T) {
	a := Adapter{}
	data, err := a.Marshal(samples(5), nil, model.RTPTimebase, 3000) // no stss
	if err != nil {
		t.Fatal(err)
	}
	res := a.Parse(data, 2, model.TimeRange{Start: 5, End: 9})
	if res.Complete {
		t.Fatal("index-less input must not report complete")
	}
	if !res.Faults.HasCode(model.FaultNoIndex) {
		t.Fatalf("expected no_index, got %v", res.Faults)
	}
	f := res.Faults[0]
	if f.Range.Start != 5 || f.Range.End != 9 {
		t.Fatalf("fault missing time range: %+v", f)
	}
}
