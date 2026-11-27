package cmaf

import (
	"encoding/binary"
	"errors"
	"testing"
)

// Synthetic minimal fMP4 builders, independent of the production parser.

func fullBox(typ string, version byte, flags uint32, body []byte) []byte {
	prefix := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return boxBytes(typ, append(prefix, body...))
}

func u32b(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64b(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// buildFragment assembles a moof/mdat pair for one track with n samples of
// the given size/duration; dataOffset follows CMAF (relative to moof start).
func buildFragment(seq, trackID, tfdt uint64, timescaledVFR bool, sampleSizes, durations []uint32) []byte {
	mfhd := fullBox(TypeMfhd, 0, 0, u32b(uint32(seq)))
	tfhd := fullBox(TypeTfhd, 0, 0, u32b(uint32(trackID)))
	tfdtBox := fullBox(TypeTfdt, 1, 0, u64b(tfdt))
	flags := trunDataOffset | trunSampleDuration | trunSampleSize
	var body []byte
	body = append(body, u32b(uint32(len(sampleSizes)))...)
	body = append(body, u32b(0)...) // data offset placeholder, patched after assembly
	for i := range sampleSizes {
		body = append(body, u32b(durations[i])...)
		body = append(body, u32b(sampleSizes[i])...)
	}
	trun := fullBox(TypeTrun, 1, uint32(flags), body)
	traf := boxBytes(TypeTraf, append(append(append([]byte{}, tfhd...), tfdtBox...), trun...))
	moof := boxBytes(TypeMoof, append(append([]byte{}, mfhd...), traf...))
	var payloadLen uint32
	for _, sz := range sampleSizes {
		payloadLen += sz
	}
	payload := make([]byte, payloadLen)
	mdat := boxBytes(TypeMdat, payload)
	// Patch trun data offset: payload = fullbox(4) + count(4) + dataOffset(4).
	dataOff := int32(len(moof) + 8)
	stream := append(moof, mdat...)
	trunStart := indexOf(stream, []byte(TypeTrun)) - 4
	binary.BigEndian.PutUint32(stream[trunStart+8+8:], uint32(dataOff))
	_ = timescaledVFR
	return stream
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+4 <= len(haystack); i++ {
		if string(haystack[i:i+4]) == string(needle) {
			return i
		}
	}
	return -1
}

func TestParseFragmentRoundTrip(t *testing.T) {
	stream := buildFragment(5, 1, 9000, true, []uint32{10, 20, 30}, []uint32{3003, 1001, 2002})
	frags, gaps, err := ParseFragmentStream(stream)
	if err != nil {
		t.Fatalf("ParseFragmentStream: %v", err)
	}
	if len(frags) != 1 || len(gaps) != 0 {
		t.Fatalf("frags=%d gaps=%v", len(frags), gaps)
	}
	f := frags[0]
	if f.SequenceNumber != 5 || f.Mdat.Type != TypeMdat {
		t.Fatalf("fragment header wrong: %+v", f)
	}
	tf := f.Tracks[0]
	if tf.TrackID != 1 || tf.BaseMediaDecodeTime != 9000 {
		t.Fatalf("tfhd/tfdt wrong: %+v", tf)
	}
	if len(tf.Samples) != 3 || tf.Samples[1].Size != 20 || tf.Samples[1].Duration != 1001 {
		t.Fatalf("trun samples wrong: %+v", tf.Samples)
	}
	if !tf.HasDataOffset || tf.DataOffset != int32(len(f.Moof.Data)+8+8) {
		t.Fatalf("data offset wrong: %+v", tf)
	}
}

func TestSequenceGapsAndConflicts(t *testing.T) {
	f1 := buildFragment(10, 1, 0, false, []uint32{4}, []uint32{100})
	f3 := buildFragment(12, 1, 200, false, []uint32{4}, []uint32{100})
	frags, gaps, err := ParseFragmentStream(append(f1, f3...))
	if err != nil || len(frags) != 2 {
		t.Fatalf("gap scan: %v", err)
	}
	if len(gaps) != 1 || gaps[0] != 11 {
		t.Fatalf("gaps = %v, want [11]", gaps)
	}
	// Duplicate sequence number must be a hard error with an offset.
	dup := buildFragment(10, 1, 300, false, []uint32{4}, []uint32{100})
	_, _, err = ParseFragmentStream(append(append([]byte{}, f1...), dup...))
	var be *BoxError
	if !errors.As(err, &be) || be.BoxType != TypeMfhd {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestSampleOutsideMdat(t *testing.T) {
	stream := buildFragment(1, 1, 0, false, []uint32{4, 4}, []uint32{100, 100})
	trunStart := indexOf(stream, []byte(TypeTrun)) - 4
	// Corrupt the data offset (payload: fullbox 4 + count 4 + dataOffset 4).
	binary.BigEndian.PutUint32(stream[trunStart+8+8:], uint32(len(stream)-2))
	if _, _, err := ParseFragmentStream(stream); err == nil {
		t.Fatal("sample outside mdat must be rejected")
	}
}

func TestTruncatedTrun(t *testing.T) {
	stream := buildFragment(1, 1, 0, false, []uint32{4}, []uint32{100})
	// Shrink the trun box's declared size by 2 (full box + count survive).
	trunStart := indexOf(stream, []byte(TypeTrun)) - 4
	old := binary.BigEndian.Uint32(stream[trunStart:])
	binary.BigEndian.PutUint32(stream[trunStart:], old-2)
	if _, _, err := ParseFragmentStream(stream); err == nil {
		t.Fatal("truncated trun must be rejected with an offset")
	}
}

func TestTfdtVersions(t *testing.T) {
	v0 := fullBox(TypeTfdt, 0, 0, u32b(42))
	v1 := fullBox(TypeTfdt, 1, 0, u64b(1<<40))
	if got, err := parseTfdt(mustBox(t, v0)); err != nil || got != 42 {
		t.Fatalf("tfdt v0 = %d,%v", got, err)
	}
	if got, err := parseTfdt(mustBox(t, v1)); err != nil || got != 1<<40 {
		t.Fatalf("tfdt v1 = %d,%v", got, err)
	}
}

func mustBox(t *testing.T, raw []byte) Box {
	t.Helper()
	boxes, err := ScanBoxes(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	return boxes[0]
}
