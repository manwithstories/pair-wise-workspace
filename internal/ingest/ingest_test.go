package ingest_test

import (
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/fault"
	"media-pipeline/internal/ingest"
	"media-pipeline/internal/media"

	"github.com/pion/rtp"
)

var tb = clock.Timebase{SampleRate: 48000, Channels: 1, BitsPerSample: 16}

func mkPkt(seq uint16) *ingest.ParsedPacket {
	p := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, PayloadType: 96},
		Payload: make([]byte, 2*4), // 4 帧/包
	}
	return &ingest.ParsedPacket{Packet: p, Frames: 4}
}

func pushAll(t *testing.T, r *ingest.Reorderer, pkts ...*ingest.ParsedPacket) []ingest.Event {
	t.Helper()
	var evs []ingest.Event
	for _, p := range pkts {
		got, err := r.Push(p)
		if err != nil {
			t.Fatalf("push seq %d: %v", p.SequenceNumber, err)
		}
		evs = append(evs, got...)
	}
	return evs
}

func seqs(evs []ingest.Event) []uint16 {
	var out []uint16
	for _, e := range evs {
		if e.Kind == ingest.EventPacket {
			out = append(out, e.Packet.SequenceNumber)
		}
	}
	return out
}

func TestInOrderAndReorder(t *testing.T) {
	r := ingest.NewReorderer(ingest.ReorderConfig{WindowSize: 8, MaxSeqGap: 16, FramesPer: 4, BlockID: "b"})
	evs := pushAll(t, r, mkPkt(0), mkPkt(1), mkPkt(3), mkPkt(2))
	if got := seqs(evs); len(got) != 4 || got[0] != 0 || got[3] != 3 {
		t.Fatalf("ordered seq = %v, want [0 1 2 3]", got)
	}
}

func TestDuplicateDedup(t *testing.T) {
	r := ingest.NewReorderer(ingest.ReorderConfig{WindowSize: 8, MaxSeqGap: 16, FramesPer: 4, BlockID: "b"})
	evs := pushAll(t, r, mkPkt(0), mkPkt(0), mkPkt(1), mkPkt(1))
	if got := seqs(evs); len(got) != 2 {
		t.Fatalf("dedup seq = %v, want exactly [0 1]", got)
	}
}

func TestBurstGapExpiry(t *testing.T) {
	r := ingest.NewReorderer(ingest.ReorderConfig{WindowSize: 8, MaxSeqGap: 16, FramesPer: 4, BlockID: "b"})
	pushAll(t, r, mkPkt(0), mkPkt(1))
	pushAll(t, r, mkPkt(4)) // 2、3 突发丢失
	evs := r.ExpireGaps()
	var gap *media.Gap
	var ordered []uint16
	for i := range evs {
		if evs[i].Kind == ingest.EventGap {
			gap = &evs[i].Gap
		}
		if evs[i].Kind == ingest.EventPacket {
			ordered = append(ordered, evs[i].Packet.SequenceNumber)
		}
	}
	if gap == nil {
		t.Fatal("missing gap event")
	}
	if gap.FirstSample != 8 || gap.Frames != 8 || gap.FirstSeq != 2 || gap.LastSeq != 3 {
		t.Fatalf("gap = %+v, want sample=8 frames=8 seq[2..3]", gap)
	}
	if len(ordered) != 1 || ordered[0] != 4 {
		t.Fatalf("post-gap ordered = %v, want [4]", ordered)
	}

	// 缺口关闭后迟到的包必须记为 Late，不覆盖状态。
	late, err := r.Push(mkPkt(2))
	if err != nil || len(late) != 1 || late[0].Kind != ingest.EventLate {
		t.Fatalf("late packet = %#v err=%v, want EventLate", late, err)
	}
}

func TestWindowOverflowRejected(t *testing.T) {
	r := ingest.NewReorderer(ingest.ReorderConfig{WindowSize: 4, MaxSeqGap: 4, FramesPer: 4, BlockID: "b"})
	pushAll(t, r, mkPkt(0))
	_, err := r.Push(mkPkt(20))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindGap || !fe.Recoverable {
		t.Fatalf("overflow err = %v, want recoverable KindGap", err)
	}
}

func TestSequenceWrap(t *testing.T) {
	r := ingest.NewReorderer(ingest.ReorderConfig{WindowSize: 8, MaxSeqGap: 32, FramesPer: 4, BlockID: "b"})
	evs := pushAll(t, r, mkPkt(65535), mkPkt(0))
	if got := seqs(evs); len(got) != 2 || got[0] != 65535 || got[1] != 0 {
		t.Fatalf("wrap seq = %v, want [65535 0]", got)
	}
}

func TestParseUnalignedPayload(t *testing.T) {
	raw := (&rtp.Packet{Header: rtp.Header{SequenceNumber: 1}, Payload: []byte{1, 2, 3}})
	buf, err := raw.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingest.ParsePacket(buf, tb)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindMalformedInput {
		t.Fatalf("parse err = %v, want malformed input", err)
	}
}

func TestParseRoundTrip(t *testing.T) {
	src := &rtp.Packet{Header: rtp.Header{SequenceNumber: 9, PayloadType: 96, Timestamp: 480}, Payload: make([]byte, 8)}
	buf, err := src.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ingest.ParsePacket(buf, tb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.SequenceNumber != 9 || p.Frames != 4 {
		t.Fatalf("parsed = seq %d frames %d", p.SequenceNumber, p.Frames)
	}
}
