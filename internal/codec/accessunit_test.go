package codec

import (
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

func opusTOC(config, code int) byte { return byte(config<<3) | byte(code&0x3) }

func TestOpusFrameSamples(t *testing.T) {
	cases := []struct {
		payload []byte
		want    int
	}{
		{[]byte{opusTOC(16, 0)}, 120},   // CELT 2.5ms
		{[]byte{opusTOC(17, 0)}, 240},   // CELT 5ms
		{[]byte{opusTOC(20, 0)}, 120},   // CELT 2.5ms (config 20)
		{[]byte{opusTOC(0, 0)}, 480},    // SILK 10ms
		{[]byte{opusTOC(1, 0)}, 960},    // SILK 20ms
		{[]byte{opusTOC(3, 0)}, 2880},   // SILK 60ms
		{[]byte{opusTOC(1, 1)}, 1920},   // code 1: two 20ms frames
		{[]byte{opusTOC(0, 3), 4}, 1920}, // code 3: four 10ms frames
		{nil, 0},                         // zero-duration keep-alive
	}
	for i, c := range cases {
		got, err := FrameSamples(Opus, c.payload)
		if err != nil || got != c.want {
			t.Errorf("case %d: got %d, %v; want %d", i, got, err, c.want)
		}
	}
}

func TestAACFrameSamples(t *testing.T) {
	n, err := FrameSamples(AAC, []byte{0x00, 0x10})
	if err != nil || n != 1024 {
		t.Fatalf("AAC frame = %d, %v; want 1024", n, err)
	}
	if n, _ := FrameSamples(AAC, nil); n != 0 {
		t.Fatalf("empty AAC payload = %d, want 0", n)
	}
}

func TestCorruptOpusCode3(t *testing.T) {
	if _, err := FrameSamples(Opus, []byte{opusTOC(0, 3)}); fault.KindOf(err) != fault.KindCorrupt {
		t.Fatalf("truncated code-3 packet: %v", err)
	}
	if _, err := FrameSamples(Opus, []byte{opusTOC(0, 3), 0}); fault.KindOf(err) != fault.KindCorrupt {
		t.Fatalf("zero frame count: %v", err)
	}
}

func mk(seq uint16, ts uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts},
		Payload: payload,
	}
}

func TestParseVariableDurations(t *testing.T) {
	pkts := []*rtp.Packet{
		mk(0, 0, opusTOC(1, 0)),    // 960 samples
		mk(1, 960, opusTOC(16, 0)), // 120 samples
		mk(2, 1080),                // zero-duration keep-alive
		mk(3, 1080, opusTOC(1, 0)), // 960 samples
	}
	p, err := Parse(pkts, Opus, clock.Base48k)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 4 {
		t.Fatalf("units=%d, want 4", len(p.Units))
	}
	if p.Units[1].Start.Ticks != 960 || p.Units[1].Dur.Ticks != 120 {
		t.Fatalf("unit1 = %+v", p.Units[1])
	}
	if p.Units[2].Dur.Ticks != 0 {
		t.Fatalf("zero-duration unit lost: %+v", p.Units[2])
	}
	if p.Units[3].Start.Ticks != 1080 {
		t.Fatalf("unit3 start = %d, want 1080", p.Units[3].Start.Ticks)
	}
}

func TestParseTimebaseRegression(t *testing.T) {
	pkts := []*rtp.Packet{
		mk(0, 960, opusTOC(1, 0)),
		mk(1, 480, opusTOC(1, 0)), // moves backwards
	}
	if _, err := Parse(pkts, Opus, clock.Base48k); fault.KindOf(err) != fault.KindTimebase {
		t.Fatalf("want timebase regression, got %v", err)
	}
}

func TestParseTimestampGap(t *testing.T) {
	pkts := []*rtp.Packet{
		mk(0, 0, opusTOC(1, 0)),
		mk(1, 48000*5, opusTOC(1, 0)), // 5s jump: burst loss
	}
	p, err := Parse(pkts, Opus, clock.Base48k)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TimestampGaps) != 1 || p.TimestampGaps[0].Ticks != 960 {
		t.Fatalf("gaps=%+v, want one gap starting at 960", p.TimestampGaps)
	}
}
