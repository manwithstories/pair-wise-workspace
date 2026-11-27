package ingest

import (
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"mediad/internal/media"
)

func mkRTP(t *testing.T, ssrc uint32, seq uint16, ts uint32) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{
		Header: rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq,
			Timestamp: ts, PayloadType: 96},
		Payload: []byte{1, 2, 3},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestExtendedSeqWrap(t *testing.T) {
	r := NewRegistry()
	r.Register(TrackParams{SSRC: 7, ClockRate: 48000})
	want := []uint64{65534, 65535, 65536, 65537}
	for i, seq := range []uint16{65534, 65535, 0, 1} {
		p, err := r.Ingest(mkRTP(t, 7, seq, uint32(960*i)), int64(i))
		if err != nil {
			t.Fatal(err)
		}
		if p.ExtSeq != want[i] {
			t.Fatalf("seq %d: got extseq %d, want %d", seq, p.ExtSeq, want[i])
		}
	}
}

func TestParseGarbageLocated(t *testing.T) {
	r := NewRegistry()
	r.Register(TrackParams{SSRC: 7, ClockRate: 48000})
	_, err := r.Ingest([]byte{0xff, 0x00, 0x13}, 42)
	if !media.IsFault(err, media.FaultParse) {
		t.Fatalf("want parse fault, got %v", err)
	}
	if err.At != 42 {
		t.Fatalf("fault must carry arrival index 42, got %v", err)
	}
}

func TestUnknownSSRC(t *testing.T) {
	r := NewRegistry()
	r.Register(TrackParams{SSRC: 7, ClockRate: 48000})
	_, err := r.Ingest(mkRTP(t, 99, 1, 1), 0)
	if !media.IsFault(err, media.FaultParse) {
		t.Fatalf("want parse fault for unknown SSRC, got %v", err)
	}
}

func mkSR(t *testing.T, ssrc uint32, ntp uint64, rtpTS uint32) []byte {
	t.Helper()
	raw, err := (&rtcp.SenderReport{SSRC: ssrc, NTPTime: ntp, RTPTime: rtpTS}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ntp(sec uint64) uint64 { return (sec + 2208988800) << 32 }

func TestSenderReportMapping(t *testing.T) {
	maps := map[uint32]*ClockMap{7: {SSRC: 7, Rate: 48000}}
	// Anchor: NTP second 1000 <-> RTP tick 48000.
	faults, err := ParseRTCP(mkSR(t, 7, ntp(1000), 48000), 0, maps)
	if err != nil || len(faults) != 0 {
		t.Fatalf("first SR: faults=%v err=%v", faults, err)
	}
	// One second later the RTP clock advanced by exactly 48000 ticks.
	faults, err = ParseRTCP(mkSR(t, 7, ntp(1001), 96000), 1, maps)
	if err != nil || len(faults) != 0 {
		t.Fatalf("steady SR: faults=%v err=%v", faults, err)
	}
	got := maps[7].ToMediaTime(96000 + 24000) // half a second past anchor
	want := media.FromTicks(96000+24000-96000, 48000).
		AddNanos(NTPTimespecToNanos(ntp(1001)))
	if media.Compare(got, want) != 0 {
		t.Fatalf("mapping mismatch: got %v want %v", got, want)
	}
	if got.Rem != 0 || got.Rate != 48000 {
		t.Fatalf("remainder not preserved: %+v", got)
	}
}

func TestClockJumpDetected(t *testing.T) {
	maps := map[uint32]*ClockMap{7: {SSRC: 7, Rate: 48000}}
	if _, err := ParseRTCP(mkSR(t, 7, ntp(1000), 48000), 0, maps); err != nil {
		t.Fatal(err)
	}
	// NTP advanced 1s but RTP advanced 3s: a jump.
	faults, err := ParseRTCP(mkSR(t, 7, ntp(1001), 48000+3*48000), 5, maps)
	if err != nil {
		t.Fatal(err)
	}
	if len(faults) != 1 || faults[0].Kind != media.FaultClockJump {
		t.Fatalf("want one clock-jump fault, got %v", faults)
	}
	if faults[0].At != 5 {
		t.Fatalf("fault must carry arrival index 5, got %d", faults[0].At)
	}
	if maps[7].Jumps != 1 {
		t.Fatalf("jump counter = %d", maps[7].Jumps)
	}
	// After re-anchoring the mapping follows the new report.
	got := maps[7].ToMediaTime(48000 + 3*48000)
	want := media.FromTicks(0, 48000).AddNanos(NTPTimespecToNanos(ntp(1001)))
	if media.Compare(got, want) != 0 {
		t.Fatalf("re-anchor mismatch: got %v want %v", got, want)
	}
}
