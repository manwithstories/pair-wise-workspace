package sample

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11}
	raw := Frame(7, data)
	if len(raw) != FrameHeaderLen+len(data) {
		t.Fatalf("frame length %d", len(raw))
	}
	tid, payload, _, ok := ParseFrame(raw)
	if !ok || tid != 7 || !bytes.Equal(payload, data) {
		t.Fatalf("round trip failed ok=%v tid=%d", ok, tid)
	}
}

func TestParseFrameCorruption(t *testing.T) {
	raw := Frame(3, []byte("payload-bytes"))
	raw[FrameHeaderLen+2] ^= 0xFF // flip a payload byte
	_, _, bad, ok := ParseFrame(raw)
	if ok || bad != 12 {
		t.Fatalf("CRC corruption: ok=%v badOffset=%d, want 12", ok, bad)
	}
	raw2 := Frame(3, []byte("x"))
	raw2[0] = 'Z' // break magic -> offset 0
	_, _, bad2, ok2 := ParseFrame(raw2)
	if ok2 || bad2 != 0 {
		t.Fatalf("magic corruption: ok=%v bad=%d", ok2, bad2)
	}
}

func TestParseFrameTruncation(t *testing.T) {
	raw := Frame(3, []byte("hello"))
	if _, _, _, ok := ParseFrame(raw[:6]); ok {
		t.Fatal("short buffer must fail")
	}
	// Claimed length exceeds available bytes.
	raw[7] = 0xFF
	_, _, bad, ok := ParseFrame(raw)
	if ok || bad != 4 {
		t.Fatalf("length overrun: ok=%v bad=%d want 4", ok, bad)
	}
}
