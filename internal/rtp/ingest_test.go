package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"

	"mediarchive/internal/clock"
	"mediarchive/internal/event"
)

var testCfg = event.CodecConfig{Codec: event.CodecOpus, ClockRate: 48000, Channels: 2}

func marshal(t *testing.T, ssrc uint32, seq uint16, ts uint32, payload []byte) []byte {
	t.Helper()
	raw, err := (&pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq, Timestamp: ts},
		Payload: payload,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func opusPayload(t *testing.T) []byte {
	t.Helper()
	return []byte{19<<3 | 0x4, 1, 2, 3} // CELT 20 ms stereo, code 0
}

func TestSeqAndTimestampWrap(t *testing.T) {
	r := NewReceiver(7)
	// Start near both wrap points and cross them.
	seqs := []uint16{65534, 65535, 0, 1}
	for i, s := range seqs {
		ts := uint32(0xFFFFFF00) + uint32(i)*960 // wraps to small values
		p, q, err := r.Accept(marshal(t, 7, s, ts, opusPayload(t)), 0, testCfg)
		if err != nil || q != nil {
			t.Fatalf("packet %d: err=%v q=%v", i, err, q)
		}
		wantSeq := uint64(65534 + i)
		if p.SeqExt != wantSeq {
			t.Fatalf("packet %d: SeqExt=%d want %d", i, p.SeqExt, wantSeq)
		}
		wantTS := uint64(0xFFFFFF00) + uint64(i)*960
		if p.TSExt != wantTS {
			t.Fatalf("packet %d: TSExt=%d want %d", i, p.TSExt, wantTS)
		}
		if p.MediaTime != clock.FromTicks(wantTS, 48000) {
			t.Fatalf("packet %d: MediaTime=%d", i, p.MediaTime)
		}
	}
}

func TestDuplicateIdempotent(t *testing.T) {
	r := NewReceiver(7)
	raw := marshal(t, 7, 100, 4800, opusPayload(t))
	p1, _, _ := r.Accept(raw, 0, testCfg)
	p2, q2, err2 := r.Accept(raw, 0, testCfg)
	p3, _, _ := r.Accept(raw, 0, testCfg)
	if p1 == nil {
		t.Fatal("first accept dropped")
	}
	if p2 != nil || q2 != nil || err2 != nil || p3 != nil {
		t.Fatal("duplicate not suppressed idempotently")
	}
	if r.Stats.Duplicates != 2 {
		t.Fatalf("duplicates=%d want 2", r.Stats.Duplicates)
	}
}

func TestSSRCIsolation(t *testing.T) {
	r := NewReceiver(7)
	p, q, err := r.Accept(marshal(t, 99, 1, 960, opusPayload(t)), 0, testCfg)
	if p != nil || q != nil || err != nil {
		t.Fatal("foreign SSRC not isolated")
	}
	if r.Stats.Foreign != 1 {
		t.Fatalf("foreign=%d want 1", r.Stats.Foreign)
	}
}

func TestTruncatedAndUndecodableQuarantine(t *testing.T) {
	r := NewReceiver(7)
	// Truncated Opus payload (empty) -> quarantine with reason.
	_, q, err := r.Accept(marshal(t, 7, 1, 960, nil), 0, testCfg)
	if err != nil || q == nil || q.Reason != event.ReasonTruncated {
		t.Fatalf("truncated: q=%+v err=%v", q, err)
	}
	// Undecodable codec config -> error naming the reason.
	bad := event.CodecConfig{Codec: event.CodecUnknown, ClockRate: 48000, Channels: 2}
	_, _, err = r.Accept(marshal(t, 7, 2, 1920, opusPayload(t)), 0, bad)
	ee, ok := err.(*event.Error)
	if !ok || ee.Reason != event.ReasonUndecodableConfig {
		t.Fatalf("undecodable config: err=%v", err)
	}
}

func TestVariablePacketDuration(t *testing.T) {
	// Opus durations derive from the TOC, not a fixed sample count.
	cases := []struct {
		payload []byte
		want    uint64
	}{
		{[]byte{16 << 3}, 120},               // CELT 2.5 ms
		{[]byte{17 << 3}, 240},               // CELT 5 ms
		{[]byte{19 << 3}, 960},               // CELT 20 ms
		{[]byte{0 << 3}, 480},                // SILK 10 ms
		{[]byte{3<<3 | 0x3, 5}, 5 * 2880},    // SILK 60 ms x5 frames
		{[]byte{19<<3 | 0x1, 0, 0}, 2 * 960}, // two 20 ms frames
	}
	for i, c := range cases {
		got, err := PacketDuration(testCfg, c.payload)
		if err != nil || got != c.want {
			t.Fatalf("case %d: got=%d err=%v want=%d", i, got, err, c.want)
		}
	}
	// AAC: AU-header table of 3 entries -> 3 x 1024 samples.
	aacCfg := event.CodecConfig{Codec: event.CodecAAC, ClockRate: 48000, Channels: 2}
	threeAU := append([]byte{0, 48}, make([]byte, 6+30)...)
	got, err := PacketDuration(aacCfg, threeAU)
	if err != nil || got != 3072 {
		t.Fatalf("aac 3 AU: got=%d err=%v", got, err)
	}
	// AAC single AU without header table.
	got, err = PacketDuration(aacCfg, []byte{0, 0, 1, 2, 3})
	if err != nil || got != 1024 {
		t.Fatalf("aac 1 AU: got=%d err=%v", got, err)
	}
	// AAC truncated payload -> explicit failure.
	if _, err = PacketDuration(aacCfg, []byte{0}); err == nil {
		t.Fatal("aac truncated payload accepted")
	}
}
