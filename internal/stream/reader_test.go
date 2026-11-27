package stream

import (
	"bytes"
	"testing"

	"avflow/internal/model"
)

var tb90k = model.Rational{Num: 1, Den: 90000}

func videoTrack(id uint32, maxKeyNS int64) TrackDesc {
	return TrackDesc{ID: id, Kind: KindVideo, TimeBase: tb90k, ColorTag: ColorSRGB,
		Width: 64, Height: 48, MaxKeyIntervalNS: maxKeyNS, Name: "vid"}
}

func build(t *testing.T, tracks []TrackDesc, pkts []PacketSpec) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := Encode(&buf, tracks, pkts); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return &buf
}

func TestRoundTripAndCRC(t *testing.T) {
	pkts := []PacketSpec{
		{Track: 1, Seq: 0, PTS: 0, Keyframe: true, Payload: []byte("frame0")},
		{Track: 1, Seq: 1, PTS: 3000, Payload: []byte("frame1")},
		{Track: 1, Seq: 2, PTS: 6000, Keyframe: true, Payload: []byte("frame2")},
	}
	buf := build(t, []TrackDesc{videoTrack(1, 0)}, pkts)
	r, err := NewReader(buf, 0)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	idx, err := BuildIndex(r)
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	if len(idx.Packets) != 3 {
		t.Fatalf("packets = %d, want 3", len(idx.Packets))
	}
	// 3000 ticks at 1/90000 s = 1/30 s.
	if got := idx.Packets[1].PTSNS; got != 33_333_333 {
		t.Fatalf("pts ns = %d, want 33333333", got)
	}
	if len(idx.Keyframes[1]) != 2 || idx.Keyframes[1][1].Seq != 2 {
		t.Fatalf("keyframes = %+v", idx.Keyframes[1])
	}
	if len(idx.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", idx.Faults)
	}
	if idx.FirstCorruptGlobal != -1 {
		t.Fatalf("clean stream marked corrupt at %d", idx.FirstCorruptGlobal)
	}
}

func TestBadCRCLocalizesAndCrops(t *testing.T) {
	pkts := []PacketSpec{
		{Track: 1, Seq: 0, PTS: 0, Keyframe: true, Payload: []byte("ok0")},
		{Track: 1, Seq: 1, PTS: 3000, Payload: []byte("bad"), CorruptCRC: true},
		{Track: 1, Seq: 2, PTS: 6000, Keyframe: true, Payload: []byte("ok2")},
	}
	buf := build(t, []TrackDesc{videoTrack(1, 0)}, pkts)
	idx, err := BuildIndex(mustReader(t, buf))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if idx.FirstFault == nil || idx.FirstFault.Type != model.FaultBadCRC || idx.FirstFault.PacketSeq != 1 {
		t.Fatalf("first fault = %+v", idx.FirstFault)
	}
	if idx.FirstCorruptGlobal != 1 {
		t.Fatalf("corrupt order = %d, want 1", idx.FirstCorruptGlobal)
	}
	if got := len(idx.ValidPackets()); got != 1 {
		t.Fatalf("valid prefix = %d, want 1", got)
	}
	if !idx.BadCRCSeqs[1][1] {
		t.Fatal("bad-crc sequence not recorded")
	}
}

func TestPTSRegression(t *testing.T) {
	pkts := []PacketSpec{
		{Track: 1, Seq: 0, PTS: 3000, Keyframe: true, Payload: []byte("a")},
		{Track: 1, Seq: 1, PTS: 0, Payload: []byte("b")},
	}
	buf := build(t, []TrackDesc{videoTrack(1, 0)}, pkts)
	idx, _ := BuildIndex(mustReader(t, buf))
	if idx.FirstFault == nil || idx.FirstFault.Type != model.FaultPTSRegression {
		t.Fatalf("want pts regression, got %+v", idx.FirstFault)
	}
	if idx.FirstFault.Recoverable {
		t.Fatal("PTS regression must be non-recoverable and trigger rollback")
	}
}

func TestMissingKeyframe(t *testing.T) {
	// 90000 ticks = 1 s; max interval 500 ms, gap is 1 s -> fault.
	pkts := []PacketSpec{
		{Track: 1, Seq: 0, PTS: 0, Keyframe: true, Payload: []byte("k0")},
		{Track: 1, Seq: 1, PTS: 45000, Payload: []byte("p1")},
		{Track: 1, Seq: 2, PTS: 90000, Keyframe: true, Payload: []byte("k2")},
	}
	buf := build(t, []TrackDesc{videoTrack(1, 500_000_000)}, pkts)
	idx, _ := BuildIndex(mustReader(t, buf))
	found := false
	for _, f := range idx.Faults {
		if f.Type == model.FaultMissingKeyframe && f.PacketSeq == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-keyframe fault not raised: %v", idx.Faults)
	}
}

func TestTruncatedTail(t *testing.T) {
	pkts := []PacketSpec{{Track: 1, Seq: 0, PTS: 0, Keyframe: true, Payload: []byte("x")}}
	buf := build(t, []TrackDesc{videoTrack(1, 0)}, pkts)
	raw := append([]byte(nil), buf.Bytes()...)
	half := raw[:len(raw)-3] // cut inside the payload
	idx, err := BuildIndex(mustReader(t, bytes.NewBuffer(half)))
	if err == nil {
		t.Fatal("truncated stream must surface an error")
	}
	if idx.FirstFault == nil || idx.FirstFault.Type != model.FaultTruncated {
		t.Fatalf("want truncated fault, got %+v", idx.FirstFault)
	}
	if len(idx.ValidPackets()) != 0 {
		t.Fatal("no packet may survive a truncated tail")
	}
}

func TestPayloadLimitAndLostBoundary(t *testing.T) {
	pkts := []PacketSpec{{Track: 1, Seq: 0, PTS: 0, Keyframe: true, Payload: make([]byte, 64)}}
	buf := build(t, []TrackDesc{videoTrack(1, 0)}, pkts)
	r, err := NewReader(bytes.NewReader(buf.Bytes()), 32)
	if err != nil {
		t.Fatalf("reader header: %v", err)
	}
	if _, err := r.Next(); err == nil {
		t.Fatal("oversized payload accepted")
	}
	// Flip the sync marker of the only packet.
	b := append([]byte(nil), buf.Bytes()...)
	b[headerSize] = 0x00
	r2, err := NewReader(bytes.NewReader(b), 0)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := r2.Next(); err == nil {
		t.Fatal("lost packet boundary accepted")
	}
}

func mustReader(t *testing.T, buf *bytes.Buffer) *Reader {
	t.Helper()
	r, err := NewReader(bytes.NewReader(buf.Bytes()), 0)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	return r
}
