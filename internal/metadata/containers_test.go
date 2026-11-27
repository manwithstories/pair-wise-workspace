package metadata

import (
	"encoding/binary"
	"testing"

	"fastpost/internal/model"
)

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func TestParseSubtitleRoundTrip(t *testing.T) {
	var data []byte
	data = append(data, u32(3003)...)
	data = append(data, u32(1500)...)
	data = append(data, u16(5)...)
	data = append(data, "hello"...)
	got, err := ParseSubtitle("sub1", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Event.Position != 3003 || got[0].Event.Duration != 1500 {
		t.Fatalf("pos/dur = %d/%d", got[0].Event.Position, got[0].Event.Duration)
	}
	if got[0].Event.Kind != KindSubtitle || got[0].Event.Track != "sub1" {
		t.Fatalf("kind/track wrong: %+v", got[0].Event)
	}
}

func TestParseSubtitleTruncated(t *testing.T) {
	// Header claims 8 text bytes but supplies 2.
	var data []byte
	data = append(data, u32(0)...)
	data = append(data, u32(10)...)
	data = append(data, u16(8)...)
	data = append(data, "ab"...)
	_, err := ParseSubtitle("sub2", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultBoxTruncated {
		t.Fatalf("kind = %v, want box-truncated", f.Kind)
	}
	// Fault names the first byte beyond the blob.
	if f.Offset != int64(len(data)) {
		t.Fatalf("offset = %d, want %d", f.Offset, len(data))
	}
}

func TestParseChapterRejectsZeroDuration(t *testing.T) {
	var data []byte
	data = append(data, u32(0)...)
	data = append(data, u32(0)...) // zero duration illegal for chapters
	data = append(data, u16(4)...)
	data = append(data, "evac"...)
	_, err := ParseChapter("ch1", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultNegativeDuration {
		t.Fatalf("kind = %v, want negative-duration", f.Kind)
	}
}

func TestParseChapterRoundTrip(t *testing.T) {
	var data []byte
	data = append(data, u32(90)...)
	data = append(data, u32(900)...)
	data = append(data, u16(6)...)
	data = append(data, "stage2"...)
	got, err := ParseChapter("ch2", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "stage2" || got[0].Event.Kind != KindChapter {
		t.Fatalf("got %+v", got)
	}
}

func encodeID3(frames ...[]byte) []byte {
	var body []byte
	for _, fr := range frames {
		body = append(body, fr...)
	}
	out := []byte("ID3")
	out = append(out, 3, 0, 0) // version 2.3.0, flags 0
	sz := uint32(len(body))
	out = append(out, byte(sz>>21), byte(sz>>14), byte(sz>>7), byte(sz))
	return append(out, body...)
}

func txxxFrame(pos, dur uint32, tag string) []byte {
	var payload []byte
	payload = append(payload, u32(pos)...)
	payload = append(payload, u32(dur)...)
	payload = append(payload, tag...)
	var fr []byte
	fr = append(fr, "TXXX"...)
	fr = append(fr, u32(uint32(len(payload)))...)
	fr = append(fr, 0, 0) // flags
	return append(fr, payload...)
}

func TestParseID3RoundTrip(t *testing.T) {
	data := encodeID3(txxxFrame(120, 60, "cue-1"))
	got, err := ParseID3("id3a", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TagID != "cue-1" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Event.Position != 120 || got[0].Event.Kind != KindID3 {
		t.Fatalf("event wrong: %+v", got[0].Event)
	}
}

func TestParseID3BadHeader(t *testing.T) {
	if _, err := ParseID3("id3b", []byte("XXX"), testRate); err == nil {
		t.Fatal("bad ID3 header must fault")
	}
	// Declared size beyond blob.
	data := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 100}
	_, err := ParseID3("id3c", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultBoxTruncated {
		t.Fatalf("kind = %v, want box-truncated", f.Kind)
	}
}

func encodeEMSG(scheme, value string, timescale uint32, pd, dur uint64) []byte {
	var b []byte
	b = append(b, 0, 0, 0, 0) // version/flags
	b = append(b, scheme...)
	b = append(b, 0)
	b = append(b, value...)
	b = append(b, 0)
	b = append(b, u32(timescale)...)
	eb := make([]byte, 8)
	binary.BigEndian.PutUint64(eb, pd)
	b = append(b, eb...)
	binary.BigEndian.PutUint64(eb, dur)
	b = append(b, eb...)
	return b
}

func TestParseEMSGRoundTrip(t *testing.T) {
	data := encodeEMSG("urn:drill:stage", "evac", 1000, 42, 8)
	got, err := ParseEMSG("emsg1", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	e := got[0]
	if e.SchemeID != "urn:drill:stage" || e.Value != "evac" {
		t.Fatalf("scheme/value = %q/%q", e.SchemeID, e.Value)
	}
	if e.Timescale != 1000 || e.PresentationDelta != 42 || e.Event.Duration != 8 {
		t.Fatalf("numeric fields wrong: %+v", e)
	}
	if e.Event.Kind != KindEMSG {
		t.Fatalf("kind = %v", e.Event.Kind)
	}
}

func TestParseEMSGTruncated(t *testing.T) {
	// Cut inside the numeric tail.
	data := encodeEMSG("s", "v", 1000, 1, 2)
	_, err := ParseEMSG("emsg2", data[:len(data)-5], testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultBoxTruncated {
		t.Fatalf("kind = %v, want box-truncated", f.Kind)
	}
}

func TestParserVersionStable(t *testing.T) {
	if ParserVersion == "" {
		t.Fatal("parser version must be non-empty for checkpoint validation")
	}
}
