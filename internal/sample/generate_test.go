package sample

import (
	"bytes"
	"io"
	"testing"

	"github.com/pion/rtp"

	"tsbatch/internal/ts"
)

func TestGenerateDeterministic(t *testing.T) {
	o := Options{Packets: 1500, PCRWrap: true, CCGapAt: 500, PSIUpdateAt: 700, TruncateBytes: 50}
	a := Generate(o)
	b := Generate(o)
	if !bytes.Equal(a, b) {
		t.Fatal("same options must produce identical bytes")
	}
	if len(a) != 1500*ts.PacketSize+50 {
		t.Fatalf("len=%d", len(a))
	}
}

func TestGenerateValidPacketsAndCC(t *testing.T) {
	stream := Generate(Options{Packets: 800})
	rd, err := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	if err != nil {
		t.Fatal(err)
	}
	tracker := ts.NewCCTracker()
	var pkt [ts.PacketSize]byte
	n := 0
	for {
		_, err := rd.Next(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h, err := ts.ParseHeader(&pkt)
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if v, _ := tracker.Check(h.PID, h.CC, h.HasPayload); v == ts.CCGap {
			t.Fatalf("packet %d: unexpected cc gap on clean stream", n)
		}
		n++
	}
	if n != 800 {
		t.Fatalf("packets=%d", n)
	}
}

func TestGenerateInjectsCCGap(t *testing.T) {
	stream := Generate(Options{Packets: 800, CCGapAt: 100, CCGapSize: 2})
	rd, _ := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	tracker := ts.NewCCTracker()
	var pkt [ts.PacketSize]byte
	gaps := 0
	for {
		if _, err := rd.Next(&pkt); err != nil {
			break
		}
		h, _ := ts.ParseHeader(&pkt)
		if v, _ := tracker.Check(h.PID, h.CC, h.HasPayload); v == ts.CCGap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("gaps=%d want 1", gaps)
	}
}

// TestRTPTrackBoundaries verifies the pion/rtp-based data track: every
// data packet carries one well-formed RTP packet and sequence numbers
// are continuous.
func TestRTPTrackBoundaries(t *testing.T) {
	stream := Generate(Options{Packets: 800})
	rd, _ := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	var pkt [ts.PacketSize]byte
	var lastSeq uint16
	count := 0
	for {
		if _, err := rd.Next(&pkt); err != nil {
			break
		}
		h, err := ts.ParseHeader(&pkt)
		if err != nil || h.PID != PIDData || h.PayloadOff < 0 {
			continue
		}
		pes := pkt[h.PayloadOff:]
		if !bytes.HasPrefix(pes, []byte{0, 0, 1, 0xBD}) {
			t.Fatal("data packet must start a private-stream PES")
		}
		rtpRaw := pes[9:] // PES header: 6 prefix + 3 flags, no PTS
		var rp rtp.Packet
		if err := rp.Unmarshal(rtpRaw); err != nil {
			t.Fatalf("rtp boundary broken: %v", err)
		}
		if count > 0 && rp.SequenceNumber != lastSeq+1 {
			t.Fatalf("rtp sequence jump: %d -> %d", lastSeq, rp.SequenceNumber)
		}
		lastSeq = rp.SequenceNumber
		count++
	}
	if count == 0 {
		t.Fatal("no RTP packets found")
	}
}

func TestTruncateBytes(t *testing.T) {
	stream := Generate(Options{Packets: 100, TruncateBytes: 100})
	rd, _ := ts.NewReader(bytes.NewReader(stream), ts.PacketSize)
	var pkt [ts.PacketSize]byte
	for {
		_, err := rd.Next(&pkt)
		if err == nil {
			continue
		}
		te, ok := err.(*ts.TruncationError)
		if !ok || te.Partial != 100 {
			t.Fatalf("want 100-byte truncation, got %v", err)
		}
		break
	}
}
