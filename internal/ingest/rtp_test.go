package ingest

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/pion/rtp"
)

func makePacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           1,
			Marker:         true,
		},
		Payload: []byte{0xAA, 0xBB},
	}
}

func drain(t *testing.T, b *ReorderBuffer, pkts []orderedIn) []OrderedPacket {
	t.Helper()
	var out []OrderedPacket
	for _, in := range pkts {
		got, late, err := b.Push(in.p, in.src)
		if err != nil {
			t.Fatalf("push seq %d: %v", in.p.SequenceNumber, err)
		}
		if len(late) != 0 {
			t.Fatalf("unexpected late packet at seq %d", in.p.SequenceNumber)
		}
		out = append(out, got...)
	}
	return out
}

type orderedIn struct {
	p   *rtp.Packet
	src Source
}

func TestReorderInOrder(t *testing.T) {
	b := NewReorderBuffer(ReorderConfig{WindowSize: 8})
	var in []orderedIn
	for s := uint16(0); s < 10; s++ {
		in = append(in, orderedIn{makePacket(s, uint32(s)*3600), SourceLive})
	}
	out := drain(t, b, in)
	if len(out) != 10 {
		t.Fatalf("released %d, want 10", len(out))
	}
	for i, o := range out {
		if o.IsGap || o.Packet.SequenceNumber != uint16(i) || o.Source != SourceLive {
			t.Fatalf("pos %d wrong: %+v", i, o)
		}
	}
	st := b.Stats()
	if st.Received != 10 || st.Released != 10 || st.Gaps != 0 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

func TestReorderOutOfOrderAndRTX(t *testing.T) {
	b := NewReorderBuffer(ReorderConfig{WindowSize: 8})
	// 0,1 arrive; 2 missing; 3,4 arrive; RTX fills 2.
	out := drain(t, b, []orderedIn{
		{makePacket(0, 0), SourceLive},
		{makePacket(1, 3600), SourceLive},
		{makePacket(3, 10800), SourceLive},
		{makePacket(4, 14400), SourceLive},
		{makePacket(2, 7200), SourceRTX},
	})
	if len(out) != 5 {
		t.Fatalf("released %d, want 5", len(out))
	}
	for i, o := range out {
		if o.IsGap || o.Packet.SequenceNumber != uint16(i) {
			t.Fatalf("pos %d wrong: %+v", i, o)
		}
	}
	if !out[2].Recovered || out[2].Source != SourceRTX {
		t.Fatalf("seq 2 should be RTX recovered: %+v", out[2])
	}
	if b.Stats().RecoveredRTX != 1 {
		t.Fatalf("recovered rtx = %d, want 1", b.Stats().RecoveredRTX)
	}
}

func TestReorderGapAfterWindow(t *testing.T) {
	t.Run("gap finalized beyond window", func(t *testing.T) {
		b := NewReorderBuffer(ReorderConfig{WindowSize: 4})
		// 0 then jump to 10 (> window): 1..9 finalized as a gap, 10 released.
		out := drain(t, b, []orderedIn{
			{makePacket(0, 0), SourceLive},
			{makePacket(10, 36000), SourceLive},
		})
		if len(out) != 3 {
			t.Fatalf("outputs = %d, want packet, gap, packet", len(out))
		}
		if out[0].Packet == nil || out[0].Packet.SequenceNumber != 0 {
			t.Fatalf("first output wrong: %+v", out[0])
		}
		if !out[1].IsGap || out[1].GapStart != 1 || out[1].GapCount != 9 {
			t.Fatalf("gap wrong: %+v", out[1])
		}
		if out[2].Packet == nil || out[2].Packet.SequenceNumber != 10 {
			t.Fatalf("post-gap packet wrong: %+v", out[2])
		}
		if b.Stats().GapPackets != 9 {
			t.Fatalf("gap packets = %d, want 9", b.Stats().GapPackets)
		}
	})

	t.Run("late inside vs outside window", func(t *testing.T) {
		b := NewReorderBuffer(ReorderConfig{WindowSize: 16})
		// Establish next=11 with a fully released contiguous run.
		var in []orderedIn
		for s := uint16(0); s <= 10; s++ {
			in = append(in, orderedIn{makePacket(s, uint32(s)*3600), SourceLive})
		}
		drain(t, b, in)
		// seq 5 arrives again after release, inside the window.
		got, late, err := b.Push(makePacket(5, 18000), SourceLate)
		if err != nil || len(got) != 0 || len(late) != 1 || !late[0].Recovered {
			t.Fatalf("in-window late wrong: got=%d late=%d err=%v", len(got), len(late), err)
		}
		if b.Stats().RecoveredLate != 1 {
			t.Fatalf("recovered late = %d, want 1", b.Stats().RecoveredLate)
		}
	})

	t.Run("expired late packet", func(t *testing.T) {
		b := NewReorderBuffer(ReorderConfig{WindowSize: 4})
		drain(t, b, []orderedIn{
			{makePacket(0, 0), SourceLive},
			{makePacket(10, 36000), SourceLive}, // forces 1..9 gap, next=11
		})
		// seq 1 is 10 positions behind next=11: outside window 4.
		_, late, err := b.Push(makePacket(1, 3600), SourceRTX)
		if err != nil || len(late) != 1 || late[0].Recovered {
			t.Fatalf("expired late wrong: err=%v late=%+v", err, late)
		}
		if b.Stats().ExpiredLate != 1 {
			t.Fatalf("expired count = %d, want 1", b.Stats().ExpiredLate)
		}
	})
}

func TestReorderDuplicateAndRTX(t *testing.T) {
	b := NewReorderBuffer(ReorderConfig{WindowSize: 8})
	// seq 2 arrives while seq 1 is missing, so it stays buffered; its
	// retransmission is then a genuine duplicate, not a late arrival.
	out := drain(t, b, []orderedIn{
		{makePacket(0, 100), SourceLive},
		{makePacket(2, 300), SourceLive},
		{makePacket(2, 300), SourceRTX}, // duplicate while buffered
		{makePacket(1, 200), SourceRTX}, // fills the hole
		{makePacket(3, 400), SourceLive},
	})
	out = append(out, b.Flush()...)
	want := []uint16{0, 1, 2, 3}
	if len(out) != len(want) {
		t.Fatalf("outputs %d, want %d: %+v", len(out), len(want), out)
	}
	for i, o := range out {
		if o.IsGap || o.Packet.SequenceNumber != want[i] {
			t.Fatalf("pos %d = %+v, want seq %d", i, o, want[i])
		}
	}
	if b.Stats().Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", b.Stats().Duplicates)
	}
	// seq 1 arrived via RTX and must be attributed as recovered.
	if b.Stats().RecoveredRTX != 1 {
		t.Fatalf("recovered rtx = %d, want 1", b.Stats().RecoveredRTX)
	}
}

func TestReorder16BitWrap(t *testing.T) {
	b := NewReorderBuffer(ReorderConfig{WindowSize: 8})
	out := drain(t, b, []orderedIn{
		{makePacket(65534, 100), SourceLive},
		{makePacket(65535, 200), SourceLive},
		{makePacket(0, 300), SourceLive}, // wraps to 65536
		{makePacket(1, 400), SourceLive},
	})
	if len(out) != 4 {
		t.Fatalf("outputs = %d, want 4", len(out))
	}
	want := []uint16{65534, 65535, 0, 1}
	for i, o := range out {
		if o.IsGap || o.Packet.SequenceNumber != want[i] {
			t.Fatalf("pos %d = %+v, want seq %d", i, o, want[i])
		}
	}
}

func TestReorderFlushGaps(t *testing.T) {
	b := NewReorderBuffer(ReorderConfig{WindowSize: 16})
	released := drain(t, b, []orderedIn{
		{makePacket(0, 0), SourceLive},
		{makePacket(2, 7200), SourceLive}, // 1 missing, still inside window
	})
	// seq 0 was released immediately; Flush finalizes the 1-hole then 2.
	out := b.Flush()
	if len(out) != 2 || !out[0].IsGap || out[0].GapCount != 1 {
		t.Fatalf("flush should mark seq 1 as gap: %+v", out)
	}
	if out[1].IsGap || out[1].Packet.SequenceNumber != 2 {
		t.Fatalf("flush tail wrong: %+v", out[1])
	}
	if len(released) != 1 || released[0].Packet.SequenceNumber != 0 {
		t.Fatalf("early release wrong: %+v", released)
	}
}

func TestValidateAndParsePacket(t *testing.T) {
	raw, err := makePacket(7, 999).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePacket(raw)
	if err != nil || p.SequenceNumber != 7 {
		t.Fatalf("parse: %v %+v", err, p)
	}
	bad := *p
	bad.Version = 3
	if err := ValidatePacket(&bad); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("version check: %v", err)
	}
	empty := &rtp.Packet{Header: rtp.Header{Version: 2}}
	if err := ValidatePacket(empty); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("empty payload check: %v", err)
	}
	if _, err := ParsePacket(nil); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("nil datagram: %v", err)
	}
	jumbo := make([]byte, MaxPacketBytes+1)
	if _, err := ParsePacket(jumbo); !errors.Is(err, clock.ErrInvalidArgument) {
		t.Fatalf("jumbo datagram: %v", err)
	}
}
