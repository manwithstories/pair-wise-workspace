package isobmff

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/pairwise/relay/internal/media"
)

func encodeBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func encodeFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(head[0:4], uint32(version)<<24|(flags&0xffffff))
	copy(head[4:], payload)
	return encodeBox(typ, head)
}

func testFragment(t *testing.T) []byte {
	t.Helper()
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, 17)
	mfhd := encodeFullBox("mfhd", 0, 0, mfhdPayload)

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, 180000)
	tfdt := encodeFullBox("tfdt", 1, 0, tfdtPayload)

	trunPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(trunPayload, 2)
	do := make([]byte, 4)
	binary.BigEndian.PutUint32(do, 0)
	trunPayload = append(trunPayload, do...)
	for _, s := range []struct{ d, n uint32 }{{3000, 4}, {3000, 5}} {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b[0:4], s.d)
		binary.BigEndian.PutUint32(b[4:8], s.n)
		trunPayload = append(trunPayload, b...)
	}
	trun := encodeFullBox("trun", 0, TrunDataOffsetPresent|TrunSampleDurationPresent|TrunSampleSizePresent, trunPayload)
	traf := encodeBox("traf", append(tfdt, trun...))
	moof := encodeBox("moof", append(mfhd, traf...))
	mdat := encodeBox("mdat", make([]byte, 9))
	return append(moof, mdat...)
}

func TestParseFragmentMFHDTFDTTRUN(t *testing.T) {
	raw := testFragment(t)
	id := media.FragmentID{Rendition: "high", Segment: 17}
	frag, fault := ParseFragment(raw, id)
	if fault != nil {
		t.Fatalf("ParseFragment: %v", fault)
	}
	if frag.MFHDSequence != 17 {
		t.Fatalf("mfhd=%d want 17", frag.MFHDSequence)
	}
	if frag.TFDT != 180000 || frag.TFDTVersion != 1 {
		t.Fatalf("tfdt=%d v=%d", frag.TFDT, frag.TFDTVersion)
	}
	if len(frag.Samples) != 2 || frag.Samples[0].Duration != 3000 || frag.Samples[1].Size != 5 {
		t.Fatalf("samples=%+v", frag.Samples)
	}
	if frag.MdatPayloadLength != 9 {
		t.Fatalf("mdat length=%d want 9", frag.MdatPayloadLength)
	}
}

func TestParseFragmentLocatesTruncatedMDAT(t *testing.T) {
	raw := testFragment(t)
	cut := len(raw) - 3
	faulty := raw[:cut]
	_, fault := ParseFragment(faulty, media.FragmentID{Rendition: "mid", Segment: 18})
	if fault == nil {
		t.Fatal("truncated mdat accepted")
	}
	if fault.Code != media.CodeBoxTruncated {
		t.Fatalf("code=%s want %s: %v", fault.Code, media.CodeBoxTruncated, fault)
	}
	if fault.Offset != int64(cut) {
		t.Fatalf("damage offset=%d want first unavailable byte %d", fault.Offset, cut)
	}
	if !fault.Recoverable {
		t.Fatal("truncation should be marked recoverable via refetch/redundant copy")
	}
}

func TestRejectImpossibleBoxLength(t *testing.T) {
	raw := []byte{0, 0, 0, 4, 'f', 'o', 'o', 'r'}
	_, fault := ParseTopBoxes(raw, media.FragmentID{})
	if fault == nil || fault.Code != media.CodeBoxLength {
		t.Fatalf("fault=%v want invalid_box_length", fault)
	}
	if fault.Offset != 0 {
		t.Fatalf("offset=%d want length-field offset 0", fault.Offset)
	}
}

func TestRejectOverflowingExtendedSize(t *testing.T) {
	raw := make([]byte, 16)
	binary.BigEndian.PutUint32(raw[0:4], 1)
	copy(raw[4:8], "big!")
	binary.BigEndian.PutUint64(raw[8:16], math.MaxUint64)
	_, fault := ParseTopBoxes(raw, media.FragmentID{})
	if fault == nil || fault.Code != media.CodeIntegerOverflow {
		t.Fatalf("fault=%v want integer_overflow", fault)
	}
}

func TestTruncatedExtendedHeaderOffset(t *testing.T) {
	raw := make([]byte, 8)
	binary.BigEndian.PutUint32(raw[0:4], 1)
	copy(raw[4:8], "big!")
	_, fault := ParseTopBoxes(raw, media.FragmentID{})
	if fault == nil || fault.Code != media.CodeBoxTruncated {
		t.Fatalf("fault=%v want truncated_box", fault)
	}
	if fault.Offset != int64(len(raw)) {
		t.Fatalf("offset=%d want %d", fault.Offset, len(raw))
	}
}
