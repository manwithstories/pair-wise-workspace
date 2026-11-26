package demux

import (
	"bytes"
	"testing"

	"relaycheck/internal/clock"
	"relaycheck/internal/remux"
)

func testSegment(t *testing.T, seq uint32) []byte {
	t.Helper()
	b, err := remux.SegmentBytes(seq, []remux.OutFrag{
		{TrackID: 1, TFDT: 90000, Samples: []remux.OutSample{
			{Duration: 3000, Size: 3, Flags: 0x2000000, CTO: 6000, Data: []byte("aaa")},
			{Duration: 3000, Size: 4, Flags: 0x1010000, CTO: 0, Data: []byte("bbbb")},
		}},
		{TrackID: 2, TFDT: 48000, Samples: []remux.OutSample{
			{Duration: 960, Size: 2, Data: []byte("cc")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSegmentRoundTrip(t *testing.T) {
	data := testSegment(t, 9)
	seg, fault := ParseSegment(data, 9)
	if fault != nil {
		t.Fatal(fault)
	}
	if seg.Sequence != 9 || len(seg.Traf) != 2 {
		t.Fatalf("seg=%+v", seg)
	}
	if seg.Traf[0].TrackID != 1 || seg.Traf[0].TFDT != 90000 || !seg.Traf[0].HasTFDT {
		t.Fatalf("traf0=%+v", seg.Traf[0])
	}
	all, fault := seg.ExtractAll(9)
	if fault != nil {
		t.Fatal(fault)
	}
	if len(all) != 2 || len(all[0]) != 2 || len(all[1]) != 1 {
		t.Fatalf("samples=%v", all)
	}
	if string(all[0][0].Data) != "aaa" || string(all[0][1].Data) != "bbbb" || string(all[1][0].Data) != "cc" {
		t.Fatalf("payload mismatch")
	}
	if all[0][0].CTO != 6000 || all[0][0].Flags != 0x2000000 || all[0][1].Duration != 3000 {
		t.Fatalf("sample table not preserved: %+v", all[0][0])
	}
}

func TestParseSegmentTruncated(t *testing.T) {
	data := testSegment(t, 3)
	for _, cut := range []int{len(data) - 1, len(data) / 2, 9} {
		seg, fault := ParseSegment(data[:cut], 3)
		if seg != nil || fault == nil {
			t.Fatalf("cut=%d: expected fault", cut)
		}
		if fault.Kind != clock.FaultTruncate && fault.Kind != clock.FaultBoxBound {
			t.Fatalf("cut=%d kind=%v", cut, fault.Kind)
		}
		if fault.Offset < 0 || fault.Offset >= int64(len(data)) {
			t.Fatalf("cut=%d: first error offset %d out of range", cut, fault.Offset)
		}
	}
}

func TestParseSegmentOversizeBox(t *testing.T) {
	data := testSegment(t, 4)
	// inflate the first box (styp) size beyond the limit
	for i := 0; i < 4; i++ {
		data[i] = 0xff
	}
	seg, fault := ParseSegment(data, 4)
	if seg != nil || fault == nil {
		t.Fatal("expected fault")
	}
	if fault.Offset != 0 {
		t.Fatalf("first error offset=%d", fault.Offset)
	}
}

func TestParseSegmentSampleTableLimit(t *testing.T) {
	data := testSegment(t, 5)
	i := bytes.Index(data, []byte("trun"))
	if i < 0 {
		t.Fatal("no trun")
	}
	// sample_count sits 4 bytes after the trun box header
	off := i + 4 + 4
	data[off] = 0xff
	data[off+1] = 0xff
	data[off+2] = 0xff
	data[off+3] = 0xff
	seg, fault := ParseSegment(data, 5)
	if seg != nil || fault == nil {
		t.Fatal("expected fault")
	}
}

func TestExtractAllBounds(t *testing.T) {
	data := testSegment(t, 6)
	seg, fault := ParseSegment(data, 6)
	if fault != nil {
		t.Fatal(fault)
	}
	// corrupt the first trun data-offset to point past the mdat
	seg.Traf[0].DataOffset = 1 << 24
	if _, fault := seg.ExtractAll(6); fault == nil || fault.Kind != clock.FaultBoxBound {
		t.Fatalf("expected box-bound fault, got %v", fault)
	}
}

func TestLanguageCodec(t *testing.T) {
	if got := DecodeLanguage(EncodeLanguage("eng")); got != "eng" {
		t.Fatalf("language round trip=%q", got)
	}
}
