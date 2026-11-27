package parse

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cueforge/internal/clock"
	"cueforge/internal/model"
)

func collect(t *testing.T, parse func(Emitter) error) ([]model.Event, error) {
	t.Helper()
	var evs []model.Event
	err := parse(func(ev model.Event) error {
		evs = append(evs, ev)
		return nil
	})
	return evs, err
}

func trackErr(t *testing.T, err error) *model.TrackError {
	t.Helper()
	var te *model.TrackError
	if !errors.As(err, &te) {
		t.Fatalf("expected TrackError, got %v", err)
	}
	return te
}

func TestVTTParsesCuesWithOffsets(t *testing.T) {
	const sample = "WEBVTT\n\n00:00:00.000 --> 00:00:00.500\nhello\n\n00:00:01.000 --> 00:00:01.500\nworld\n"
	evs, err := collect(t, func(emit Emitter) error {
		return ParseVTT(context.Background(), strings.NewReader(sample), 0, emit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].Offset != 8 {
		t.Errorf("cue0 offset = %d, want 8", evs[0].Offset)
	}
	want1 := int64(strings.Index(sample, "00:00:01.000"))
	if evs[1].Offset != want1 {
		t.Errorf("cue1 offset = %d, want %d", evs[1].Offset, want1)
	}
	if evs[0].Start.Compare(clock.Must(0, 1000)) != 0 ||
		evs[0].Dur.Compare(clock.Must(500, 1000)) != 0 {
		t.Errorf("cue0 time = %s + %s", evs[0].Start, evs[0].Dur)
	}
	if evs[1].Payload != "world" {
		t.Errorf("cue1 payload = %q", evs[1].Payload)
	}
}

func TestVTTIncompleteCueKeepsOffset(t *testing.T) {
	const sample = "WEBVTT\n\n00:00:00.000 --> 00:00:00.500\n"
	_, err := collect(t, func(emit Emitter) error {
		return ParseVTT(context.Background(), strings.NewReader(sample), 2, emit)
	})
	te := trackErr(t, err)
	if te.Offset != 8 || te.Track != 2 {
		t.Errorf("error = %v, want offset 8 track 2", te)
	}
	if !strings.Contains(te.Msg, "incomplete cue") {
		t.Errorf("msg = %q", te.Msg)
	}
}

func TestVTTNegativeDurationRejected(t *testing.T) {
	const sample = "WEBVTT\n\n00:00:02.000 --> 00:00:01.000\nbackwards\n"
	_, err := collect(t, func(emit Emitter) error {
		return ParseVTT(context.Background(), strings.NewReader(sample), 0, emit)
	})
	te := trackErr(t, err)
	if !strings.Contains(te.Msg, "negative duration") {
		t.Errorf("msg = %q", te.Msg)
	}
}

func TestVTTOversizeCueRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n00:00:00.000 --> 00:00:00.500\n")
	b.WriteString(strings.Repeat("x", model.MaxRecordBytes))
	_, err := collect(t, func(emit Emitter) error {
		return ParseVTT(context.Background(), strings.NewReader(b.String()), 0, emit)
	})
	trackErr(t, err)
}

func TestChaptersJSONL(t *testing.T) {
	const sample = "{\"id\":0,\"start_ms\":0,\"end_ms\":500,\"title\":\"a\"}\n{bad json}\n"
	var n int
	err := ParseChapters(context.Background(), strings.NewReader(sample), 1,
		func(model.Event) error { n++; return nil })
	te := trackErr(t, err)
	if n != 1 {
		t.Errorf("emitted %d events, want 1", n)
	}
	wantOff := int64(strings.Index(sample, "{bad json}"))
	if te.Offset != wantOff {
		t.Errorf("offset = %d, want %d", te.Offset, wantOff)
	}
}

func putSyncsafe(b []byte, v int) {
	b[0] = byte(v >> 21 & 0x7f)
	b[1] = byte(v >> 14 & 0x7f)
	b[2] = byte(v >> 7 & 0x7f)
	b[3] = byte(v & 0x7f)
}

func buildID3(t *testing.T, payloads []string) []byte {
	t.Helper()
	var frames bytes.Buffer
	for _, p := range payloads {
		frames.WriteString("TXXX")
		sz := []byte{0, 0, 0, 0}
		putSyncsafe(sz, len(p))
		frames.Write(sz)
		frames.Write([]byte{0, 0})
		frames.WriteString(p)
	}
	var out bytes.Buffer
	out.WriteString("ID3")
	out.Write([]byte{4, 0, 0})
	tag := []byte{0, 0, 0, 0}
	putSyncsafe(tag, frames.Len())
	out.Write(tag)
	out.Write(frames.Bytes())
	return out.Bytes()
}

func TestID3TXXXFrames(t *testing.T) {
	data := buildID3(t, []string{
		"start=0/1000;dur=500/1000;label=first",
		"start=500/1000;dur=500/1000;label=second",
	})
	evs, err := collect(t, func(emit Emitter) error {
		return ParseID3(context.Background(), bytes.NewReader(data), 3, emit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Payload != "first" || evs[1].Payload != "second" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[1].Offset != int64(10+10+len("start=0/1000;dur=500/1000;label=first")) {
		t.Errorf("frame1 offset = %d", evs[1].Offset)
	}
}

func TestID3TruncatedFrame(t *testing.T) {
	data := buildID3(t, []string{"start=0/1000;dur=1/1000;label=x"})
	_, err := collect(t, func(emit Emitter) error {
		return ParseID3(context.Background(), bytes.NewReader(data[:len(data)-5]), 0, emit)
	})
	te := trackErr(t, err)
	if te.Offset != 10 {
		t.Errorf("offset = %d, want 10", te.Offset)
	}
}

func buildEMSG(pts uint64, dur uint32, value string) []byte {
	var body bytes.Buffer
	body.Write([]byte{1, 0, 0, 0})
	binary.Write(&body, binary.BigEndian, uint32(1000))
	binary.Write(&body, binary.BigEndian, pts)
	binary.Write(&body, binary.BigEndian, dur)
	binary.Write(&body, binary.BigEndian, uint32(7))
	body.WriteString("urn:test")
	body.WriteByte(0)
	body.WriteString(value)
	body.WriteByte(0)
	body.WriteString("md")
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.WriteString("emsg")
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestEMSGParse(t *testing.T) {
	box := buildEMSG(1500, 250, "hello")
	evs, err := collect(t, func(emit Emitter) error {
		return ParseEMSG(context.Background(), bytes.NewReader(box), 0, emit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Start.Compare(clock.Must(1500, 1000)) != 0 {
		t.Errorf("start = %s", evs[0].Start)
	}
	if evs[0].Payload != "hello|md" {
		t.Errorf("payload = %q", evs[0].Payload)
	}
}

func TestEMSGTruncatedKeepsOffset(t *testing.T) {
	good := buildEMSG(0, 10, "ok")
	bad := buildEMSG(32, 10, "broken")
	data := append(append([]byte{}, good...), bad[:len(bad)/2]...)
	_, err := collect(t, func(emit Emitter) error {
		return ParseEMSG(context.Background(), bytes.NewReader(data), 0, emit)
	})
	te := trackErr(t, err)
	if te.Offset != int64(len(good)) {
		t.Errorf("offset = %d, want %d", te.Offset, len(good))
	}
	if !strings.Contains(te.Msg, "truncated") {
		t.Errorf("msg = %q", te.Msg)
	}
}

func TestEMSGOverflowTimestamp(t *testing.T) {
	box := buildEMSG(1<<63, 10, "overflow")
	_, err := collect(t, func(emit Emitter) error {
		return ParseEMSG(context.Background(), bytes.NewReader(box), 0, emit)
	})
	te := trackErr(t, err)
	if !strings.Contains(te.Msg, "overflow") {
		t.Errorf("msg = %q", te.Msg)
	}
}

func buildSEINAL(t *testing.T, text string) []byte {
	t.Helper()
	payload := append(append([]byte{}, seiTestUUID...), text...)
	var b bytes.Buffer
	b.Write([]byte{0, 0, 1, 0x06, 5})
	n := len(payload)
	for n >= 255 {
		b.WriteByte(0xff)
		n -= 255
	}
	b.WriteByte(byte(n))
	b.Write(payload)
	b.WriteByte(0x80)
	return b.Bytes()
}

var seiTestUUID = []byte("0123456789abcdef")

func TestSEIParse(t *testing.T) {
	nal := buildSEINAL(t, "start=1000/1000;dur=500/1000;label=sei-one")
	evs, err := collect(t, func(emit Emitter) error {
		return ParseSEI(context.Background(), bytes.NewReader(nal), 4, emit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Payload != "sei-one" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Offset != 0 {
		t.Errorf("offset = %d", evs[0].Offset)
	}
}

func TestSEICorruptSizeKeepsOffset(t *testing.T) {
	good := buildSEINAL(t, "start=0/1000;dur=1/1000;label=ok")
	// Corrupt NAL: declared payload size overruns the unit.
	bad := []byte{0, 0, 1, 0x06, 5, 200, 1, 2, 3}
	data := append(good, bad...)
	_, err := collect(t, func(emit Emitter) error {
		return ParseSEI(context.Background(), bytes.NewReader(data), 4, emit)
	})
	te := trackErr(t, err)
	if te.Offset != int64(len(good)) {
		t.Errorf("offset = %d, want %d", te.Offset, len(good))
	}
}

func TestParseTextPayloadRejectsNegativeDur(t *testing.T) {
	_, _, _, err := parseTextPayload("start=0/1000;dur=-5/1000;label=x")
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("err = %v", err)
	}
}

func TestClockOverflowAndWraparound(t *testing.T) {
	big := clock.Must(1<<62, 1)
	if _, err := big.Add(big); !errors.Is(err, clock.ErrOverflow) {
		t.Errorf("add err = %v", err)
	}
	if err := clock.CheckMonotonic(clock.Must(5, 1), clock.Must(4, 1)); !errors.Is(err, clock.ErrWraparound) {
		t.Errorf("wraparound err = %v", err)
	}
	// Exact comparison beyond int64 cross-product range.
	a := clock.Must(1<<62, 3)
	b := clock.Must(1<<62, 3)
	if a.Compare(b) != 0 {
		t.Errorf("compare = %d", a.Compare(b))
	}
}

func ExampleTime_String() {
	fmt.Println(clock.Must(3000, 1001))
	// Output: 3000/1001
}
