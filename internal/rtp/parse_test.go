package rtp

import (
	"bytes"
	"io"
	"testing"

	"medpipe/internal/fault"
)

func baseGenOpts() GenOptions {
	return GenOptions{
		Seed:             7,
		Tracks:           2,
		PacketsPerTrack:  300,
		PayloadSize:      160,
		LossRate:         0.05,
		DupRate:          0.02,
		CorruptRate:      0.01,
		ReorderWindow:    16,
		ClockJumpTrack:   -1,
		ClockJumpSeconds: 2,
		TailLoss:         3,
		TruncateTail:     3,
	}
}

func parseAll(t *testing.T, stream []byte) (*Parser, []*Record) {
	t.Helper()
	p := NewParser(bytes.NewReader(stream), nil)
	var recs []*Record
	for {
		rec, err := p.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		recs = append(recs, rec)
	}
	return p, recs
}

func TestGenerateDeterministic(t *testing.T) {
	a, err := Generate(baseGenOpts())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(baseGenOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Stream, b.Stream) {
		t.Fatal("same seed produced different streams")
	}
	if a.SentPerTrack[0] != 300 || a.SentPerTrack[1] != 300 {
		t.Fatalf("sent counts = %v", a.SentPerTrack)
	}
}

func TestParseGeneratedStream(t *testing.T) {
	g, err := Generate(baseGenOpts())
	if err != nil {
		t.Fatal(err)
	}
	p, recs := parseAll(t, g.Stream)
	if p.Stats.BadOffsetBytes != 3 {
		t.Fatalf("BadOffsetBytes = %d, want 3", p.Stats.BadOffsetBytes)
	}
	if p.Stats.RTPPackets == 0 || p.Stats.RTCPReports == 0 {
		t.Fatalf("no records parsed: %+v", p.Stats)
	}
	if len(p.Stats.TracksSeen) != 2 {
		t.Fatalf("tracks seen = %d", len(p.Stats.TracksSeen))
	}
	// Corrupt injected packets must be located faults.
	if p.Faults().Count(fault.KindCorrupt) == 0 {
		t.Fatal("expected located corrupt faults")
	}
	var rtpRecs int
	for _, r := range recs {
		if r.Type == RecRTP {
			rtpRecs++
			if r.Packet.SSRC == 0 {
				t.Fatal("zero SSRC")
			}
		}
	}
	if rtpRecs != p.Stats.RTPPackets {
		t.Fatalf("record count mismatch")
	}
}

func TestParseTruncatedTail(t *testing.T) {
	g, err := Generate(baseGenOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Cut mid-payload of the last record.
	cut := len(g.Stream) - 20
	p, _ := parseAll(t, g.Stream[:cut])
	if p.Stats.BadOffsetBytes == 0 {
		t.Fatal("expected bad offset bytes for truncated stream")
	}
	if p.Faults().Count(fault.KindCorrupt) == 0 {
		t.Fatal("expected located truncation fault")
	}
}

func TestParseGarbageResync(t *testing.T) {
	g, err := Generate(baseGenOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Splice 5 garbage bytes into the middle of the stream.
	mid := len(g.Stream) / 2
	garbage := []byte{0xFF, 0x00, 0x77, 0x42, 0x99}
	spliced := append(append([]byte{}, g.Stream[:mid]...), append(garbage, g.Stream[mid:]...)...)
	p, _ := parseAll(t, spliced)
	if p.Stats.BadOffsetBytes == 0 {
		t.Fatal("expected resync to account garbage bytes")
	}
	if p.Stats.RTPPackets == 0 {
		t.Fatal("parser failed to recover after garbage")
	}
}

func TestSeqExtenderWrap(t *testing.T) {
	var e SeqExtender
	// Start near the 16-bit boundary.
	if got := e.Extend(65534); got != 65534 {
		t.Fatalf("first ext = %d", got)
	}
	if got := e.Extend(65535); got != 65535 {
		t.Fatalf("ext = %d", got)
	}
	if got := e.Extend(0); got != 65536 {
		t.Fatalf("wrapped ext = %d, want 65536", got)
	}
	if got := e.Extend(1); got != 65537 {
		t.Fatalf("wrapped ext = %d, want 65537", got)
	}
	// Late packet from before the wrap.
	if got := e.Extend(65535); got != 65535 {
		t.Fatalf("late ext = %d, want 65535", got)
	}
	// Duplicate of max.
	if got := e.Extend(1); got != 65537 {
		t.Fatalf("dup ext = %d, want 65537", got)
	}
	max, ok := e.MaxExtended()
	if !ok || max != 65537 {
		t.Fatalf("max = %d ok=%v", max, ok)
	}
}

func TestSenderReportRoundTrip(t *testing.T) {
	sr := &SenderReport{SSRC: 42, NTPSec: 1700000000, NTPFrac: 1 << 31,
		RTPTimestamp: 96000, PacketCount: 100, OctetCount: 16000}
	raw := MarshalSenderReport(sr)
	got, err := ParseSenderReport(raw, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *sr {
		t.Fatalf("round trip = %+v, want %+v", got, sr)
	}
	// Corrupt: bad version.
	raw[0] = 1 << 6
	if _, err := ParseSenderReport(raw, 3, 99); err == nil {
		t.Fatal("expected error for bad version")
	} else {
		fe, ok := err.(*fault.Error)
		if !ok || fe.Offset != 99 || fe.Track != 3 {
			t.Fatalf("fault not located: %v", err)
		}
	}
}

func TestGenerateLimits(t *testing.T) {
	o := baseGenOpts()
	o.Tracks = 0
	if _, err := Generate(o); err == nil {
		t.Fatal("expected track limit error")
	}
	o = baseGenOpts()
	o.PacketsPerTrack = 0
	if _, err := Generate(o); err == nil {
		t.Fatal("expected empty input error")
	}
	o = baseGenOpts()
	o.PacketsPerTrack = MaxTrackSeconds*ClockRate/TicksPerPacket + 100
	if _, err := Generate(o); err == nil {
		t.Fatal("expected duration limit error")
	}
}
