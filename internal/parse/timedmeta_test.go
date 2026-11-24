package parse

import (
	"bytes"
	"errors"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

func TestSubtitleRoundTrip(t *testing.T) {
	cues := []SubtitleCue{
		{ID: "c1", Start: 0, Dur: 90000, Text: "opening"},
		{ID: "c2", Start: 90000, Dur: 45000, Text: "solo"},
	}
	data, err := EncodeSubtitles(cues)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSubtitles(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[1].Text != "solo" || back[1].Start != 90000 || back[1].Dur != 45000 {
		t.Fatalf("roundtrip: %+v", back)
	}
	if _, err := EncodeSubtitles([]SubtitleCue{{Start: 1, Dur: -1}}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("negative duration: %v", err)
	}
	if _, err := ParseSubtitles([]byte("<tt><p begin='x'/></tt>")); err == nil {
		t.Fatal("malformed xml accepted")
	}
}

func TestChapterRoundTrip(t *testing.T) {
	chapters := []Chapter{
		{ID: "chapter-1", Start: 0, Title: "Act I"},
		{ID: "chapter-2", Start: 90000, Title: "Act II"},
		{ID: "chapter-3", Start: 180000, Title: "Act III"},
	}
	payload, err := encodeChpl(chapters)
	if err != nil {
		t.Fatal(err)
	}
	raw := RawBox{Type: [4]byte{'c', 'h', 'p', 'l'}, Size: uint64(8 + len(payload)), HeaderSize: 8, Payload: payload}
	back, err := parseChpl(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || back[2].Title != "Act III" || back[2].Start != 180000 {
		t.Fatalf("roundtrip: %+v", back)
	}
	bad := RawBox{Type: raw.Type, Size: 9, HeaderSize: 8, Payload: []byte{1}}
	if _, err := parseChpl(&bad); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("short chpl: %v", err)
	}
	overflow := RawBox{Type: raw.Type, Size: uint64(8 + len(payload)), HeaderSize: 8, Payload: append([]byte(nil), payload...)}
	overflow.Payload[8] = 9 // declare more chapters than present
	if _, err := parseChpl(&overflow); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("chapter count overflow: %v", err)
	}
}

func TestID3RoundTrip(t *testing.T) {
	tag := &ID3Tag{ID: "id-7", Text: "camera-b", Owner: "stage", Private: []byte{1, 2, 3}}
	data, err := EncodeID3(tag)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseID3(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != tag.ID || back.Text != tag.Text || back.Owner != tag.Owner {
		t.Fatalf("roundtrip: %+v", back)
	}
	if !bytes.Equal(back.Private, tag.Private) {
		t.Fatalf("private bytes: %v", back.Private)
	}
	again, err := EncodeID3(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, data) {
		t.Fatal("ID3 encoding not deterministic")
	}
	if _, err := ParseID3(data[:5]); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("short id3: %v", err)
	}
	broken := append([]byte(nil), data...)
	broken[6] = 0x80 // size no longer syncsafe
	if _, err := ParseID3(broken); !errors.Is(err, model.ErrBadLength) {
		t.Fatalf("bad id3 size: %v", err)
	}
	truncated := append([]byte(nil), data[:len(data)-1]...)
	truncated[6] = 0x7F
	if _, err := ParseID3(truncated); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("id3 length claim: %v", err)
	}
}

func TestEmsgRoundTrip(t *testing.T) {
	ev := EmsgEvent{
		ID:        "emsg-1",
		Scheme:    "urn:mediarun:cue",
		Value:     "10",
		Start:     90000,
		Duration:  45000,
		Message:   []byte("roll"),
		Timescale: 90000,
	}
	raw, err := EncodeEmsg(ev)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseEmsg(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Start != ev.Start || back.Duration != ev.Duration || string(back.Message) != "roll" {
		t.Fatalf("roundtrip: %+v", back)
	}
	if back.Scheme != ev.Scheme || back.Value != ev.Value {
		t.Fatalf("scheme: %+v", back)
	}
	if _, err := EncodeEmsg(EmsgEvent{Timescale: 0}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("zero timescale: %v", err)
	}
	// Cutting into the fixed fields must be reported, not parsed as a
	// shorter event.
	corrupt := *raw
	corrupt.Payload = append([]byte(nil), raw.Payload[:40]...)
	corrupt.Size = uint64(8 + len(corrupt.Payload))
	if _, err := ParseEmsg(&corrupt); err == nil || model.ClassOf(err) != model.ClassCorrupt {
		t.Fatalf("truncated emsg payload: %v", err)
	}
	// An unterminated scheme string is truncated input.
	noNul := *raw
	noNul.Payload = []byte{1, 0, 0, 0, 'a', 'b', 'c'}
	noNul.Size = uint64(8 + len(noNul.Payload))
	if _, err := ParseEmsg(&noNul); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("unterminated scheme: %v", err)
	}
}

func TestSEIRoundTrip(t *testing.T) {
	uuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	payload := []byte{0x00, 0x00, 0x00, 0x01, 0x02, 0x03} // includes 00 00 to test emulation prevention
	data, err := EncodeSEI(uuid, payload)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ParseSEI(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if events[0].UUID != uuid || !bytes.Equal(events[0].Payload, payload) {
		t.Fatalf("roundtrip: %+v", events[0])
	}
	again, err := EncodeSEI(events[0].UUID, events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, data) {
		t.Fatal("SEI encoding not deterministic")
	}
	// A sample with no SEI NAL is not an error.
	if evs, err := ParseSEI([]byte{0, 0, 0, 2, 0x65, 0x88}); err != nil || len(evs) != 0 {
		t.Fatalf("non-SEI sample: %v %d", err, len(evs))
	}
	// Truncated length prefix.
	if _, err := ParseSEI([]byte{0, 0, 0}); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("short nal: %v", err)
	}
	// Declared NAL length beyond the sample.
	if _, err := ParseSEI([]byte{0, 0, 0, 9, 0x06}); !errors.Is(err, model.ErrTruncated) {
		t.Fatalf("nal overrun: %v", err)
	}
}

func TestSEITruncatedPayload(t *testing.T) {
	uuid := [16]byte{1}
	data, err := EncodeSEI(uuid, []byte("abcdefghij"))
	if err != nil {
		t.Fatal(err)
	}
	// Cut the payload short while keeping the NAL length prefix honest.
	cut := append([]byte(nil), data...)
	cut[3] = byte(len(cut) - 4)
	cut = cut[:len(cut)-3]
	if _, err := ParseSEI(cut); err == nil {
		t.Fatal("truncated SEI payload accepted")
	}
}

func TestScale100ns(t *testing.T) {
	got, err := scale100ns(10000000) // one second
	if err != nil || got != clock.TickRate {
		t.Fatalf("1s: %d %v", got, err)
	}
	if _, err := scale100ns(1 << 63); err == nil {
		t.Fatal("overflow accepted")
	}
}
