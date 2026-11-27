package meta

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"mediacore/internal/clock"
)

func TestID3RoundTrip(t *testing.T) {
	var frames bytes.Buffer
	frames.WriteString("TIT2")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], 7)
	frames.Write(sz[:])
	frames.Write([]byte{0, 0})
	frames.Write([]byte{3, 'H', 'e', 'l', 'l', 'o', 0})
	frames.WriteString("PRIV")
	binary.BigEndian.PutUint32(sz[:], 4)
	frames.Write(sz[:])
	frames.Write([]byte{0, 0})
	frames.Write([]byte{1, 2, 3, 4})

	tag := make([]byte, 10)
	copy(tag, "ID3")
	tag[3], tag[4] = 3, 0
	putSyncsafe(tag[6:10], frames.Len())
	tag = append(tag, frames.Bytes()...)

	parsed, err := ParseID3(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Frames) != 2 {
		t.Fatalf("frames: %d", len(parsed.Frames))
	}
	if v := parsed.TextFrames()["TIT2"]; v != "Hello" {
		t.Fatalf("title %q", v)
	}
	out, err := parsed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, tag) {
		t.Fatalf("roundtrip mismatch\nin : %x\nout: %x", tag, out)
	}
}

func TestID3RejectsCorrupt(t *testing.T) {
	if _, err := ParseID3([]byte("ID3\x03")); !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrTruncated) {
		t.Fatalf("short header accepted: %v", err)
	}
	// declares 1000 bytes but has none
	b := make([]byte, 10)
	copy(b, "ID3")
	b[3] = 3
	putSyncsafe(b[6:10], 1000)
	if _, err := ParseID3(b); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated tag accepted: %v", err)
	}
	// frame declares more than available
	b2 := make([]byte, 10+10)
	copy(b2, "ID3")
	b2[3] = 3
	putSyncsafe(b2[6:10], 10)
	copy(b2[10:], "TIT2")
	binary.BigEndian.PutUint32(b2[14:18], 500)
	if _, err := ParseID3(b2); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated frame accepted: %v", err)
	}
	big := make([]byte, MaxRecordBytes+1)
	if _, err := ParseID3(big); !errors.Is(err, ErrOversize) {
		t.Fatalf("oversize accepted: %v", err)
	}
}

func TestEmsgRoundTrip(t *testing.T) {
	id3 := []byte("ID3\x04\x00\x00\x00\x00\x00\x00")
	e := &Emsg{
		Version: 0, SchemeIDURI: "urn:example:emsg", Value: "v1",
		Timescale: 1000, PTDelta: 250, EventDuration: 400, ID: 9, Message: id3,
	}
	enc, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseEmsg(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back.SchemeIDURI != e.SchemeIDURI || back.PTDelta != e.PTDelta || back.Message == nil {
		t.Fatalf("fields: %+v", back)
	}
	if back.PresentationTicks() != 250 {
		t.Fatalf("pts %d", back.PresentationTicks())
	}
	enc2, err := back.Encode()
	if err != nil || !bytes.Equal(enc, enc2) {
		t.Fatalf("roundtrip mismatch: %v", err)
	}

	v1 := &Emsg{Version: 1, SchemeIDURI: "s", Value: "v", Timescale: 90000, Presentation: 9000, EventDuration: 100, ID: 1, Message: []byte("x")}
	enc, err = v1.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err = ParseEmsg(enc)
	if err != nil || back.Presentation != 9000 {
		t.Fatalf("v1: %+v %v", back, err)
	}
}

func TestEmsgRejectsInvalid(t *testing.T) {
	if _, err := ParseEmsg([]byte{0, 0}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short accepted: %v", err)
	}
	bad := &Emsg{Version: 0, SchemeIDURI: "s", Timescale: 0}
	enc, _ := bad.Encode()
	if _, err := ParseEmsg(enc); !errors.Is(err, ErrMalformed) {
		t.Fatalf("zero timescale accepted: %v", err)
	}
	if _, err := (&Emsg{Version: 7}).Encode(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad version encoded: %v", err)
	}
}

func TestSEIRoundTrip(t *testing.T) {
	uuid := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	user := []byte("mc-sei-payload")
	payload := append(append([]byte{}, uuid...), user...)
	orig := &SEI{Messages: []SEIMessage{
		{Type: 5, Payload: payload},
		{Type: 300, Payload: []byte{0xff, 0x00, 0x7f}},
	}}
	enc, err := orig.Encode(false)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSEINAL(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.UUID, uuid) || string(back.UserData) != string(user) {
		t.Fatalf("uuid/user data lost: %x %q", back.UUID, back.UserData)
	}
	enc2, err := back.Encode(false)
	if err != nil || !bytes.Equal(enc, enc2) {
		t.Fatalf("roundtrip mismatch: %v\n%x\n%x", err, enc, enc2)
	}
	// With a NAL header byte (type 6) the payload must still parse.
	withHeader := append([]byte{0x06}, enc...)
	if _, err := ParseSEINAL(withHeader); err != nil {
		t.Fatalf("nal header form: %v", err)
	}
}

func TestSEIRejectsTruncated(t *testing.T) {
	orig := &SEI{Messages: []SEIMessage{{Type: 5, Payload: bytes.Repeat([]byte{7}, 40)}}}
	enc, _ := orig.Encode(false)
	for _, cut := range []int{1, 2, 5, len(enc) - 10} {
		if cut >= len(enc) {
			continue
		}
		if _, err := ParseSEINAL(enc[:cut]); err == nil {
			t.Fatalf("truncated SEI (%d bytes) accepted", cut)
		}
	}
	// Declared size larger than payload.
	over := []byte{0x05, 0xff, 0x10}
	if _, err := ParseSEINAL(over); !errors.Is(err, ErrTruncated) {
		t.Fatalf("oversize payload: %v", err)
	}
	big := make([]byte, MaxRecordBytes+2)
	if _, err := ParseSEINAL(big); !errors.Is(err, ErrOversize) {
		t.Fatalf("oversize record: %v", err)
	}
}

func TestSubtitleRoundTrip(t *testing.T) {
	payload, err := EncodeSubtitle("mov_text", "Hello edge node", []byte{0x00, 0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	text, tail, err := ParseSubtitle("mov_text", payload)
	if err != nil || text != "Hello edge node" || !bytes.Equal(tail, []byte{0, 1, 2}) {
		t.Fatalf("mov_text: %q %x %v", text, tail, err)
	}
	if _, _, err := ParseSubtitle("mov_text", []byte{0x00}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short mov_text: %v", err)
	}
	// Declares 100 bytes of text, has 2.
	if _, _, err := ParseSubtitle("mov_text", []byte{0x00, 0x64, 'a', 'b'}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("declared length accepted: %v", err)
	}
	srt, err := EncodeSubtitle("subrip", "plain", nil)
	if err != nil || string(srt) != "plain" {
		t.Fatalf("subrip: %q %v", srt, err)
	}
}

func TestChapterRoundTrip(t *testing.T) {
	chs := []Chapter{
		{ID: 1, Title: "Intro = 1; #2", StartUs: 0, EndUs: 4_000_000, Metadata: map[string]string{"lang": "en"}},
		{ID: 2, Title: "Main", StartUs: 4_000_000, EndUs: 8_000_000, Metadata: map[string]string{}},
	}
	enc, err := EncodeFFMetadata(chs)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseFFMetadata(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Title != chs[0].Title || back[1].EndUs != 8_000_000 {
		t.Fatalf("chapters: %+v", back)
	}
	if _, err := EncodeFFMetadata([]Chapter{{ID: 1, StartUs: 10, EndUs: 5}}); !errors.Is(err, clock.ErrNegativeDuration) {
		t.Fatalf("inverted chapter accepted: %v", err)
	}
}

func TestRecordSizeLimit(t *testing.T) {
	if err := checkLen(KindID3, MaxRecordBytes+1); !errors.Is(err, ErrOversize) {
		t.Fatal("limit not enforced")
	}
	if err := checkLen(KindID3, -1); !errors.Is(err, ErrMalformed) {
		t.Fatal("negative length not rejected")
	}
}
