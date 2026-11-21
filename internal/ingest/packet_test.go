package ingest

import (
	"errors"
	"testing"

	"avcheck/internal/clock"
)

func TestPacketizeParseRoundTrip(t *testing.T) {
	const ch, frames = 2, 4
	pz := NewPacketizer(111, 7, ch, frames)
	parser := NewParser(ch)
	parser.BindTrack(111, 7)

	input := make([]int32, ch*frames)
	for i := range input {
		input[i] = int32(i*1000 - 3000)
	}
	pkt, err := pz.Packetize(input)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	blk, err := parser.Parse(raw, 42)
	if err != nil {
		t.Fatal(err)
	}
	if blk.TrackID != 7 || blk.SSRC != 111 || blk.Seq != 0 || blk.ArrivalMS != 42 {
		t.Fatalf("header mismatch: %+v", blk)
	}
	if blk.Frames() != frames || blk.StartTick != 0 {
		t.Fatalf("extent mismatch: frames=%d tick=%d", blk.Frames(), blk.StartTick)
	}
	for i, v := range input {
		if blk.Samples[i] != v {
			t.Fatalf("sample[%d]=%d want %d", i, blk.Samples[i], v)
		}
	}
}

func TestTimestampWrapStaysMonotonic(t *testing.T) {
	parser := NewParser(1)
	pz := NewPacketizer(5, 1, 1, 10)
	// Force the RTP timestamp close to the 32-bit boundary.
	pz.ts = 0xFFFFFFFA
	var prev int64 = -1
	for i := 0; i < 12; i++ {
		pkt, err := pz.Packetize(make([]int32, 10))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := pkt.Marshal()
		blk, err := parser.Parse(raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		if blk.StartTick <= prev && i > 0 {
			t.Fatalf("packet %d tick %d did not advance past %d", i, blk.StartTick, prev)
		}
		prev = blk.StartTick
	}
	// After wrap the extended tick must be positive, continuing past 2^32.
	if prev <= 0xFFFFFFFF {
		t.Fatalf("extended timestamp %d did not cross 2^32", prev)
	}
}

func TestRejectMisalignedAndOversizePayload(t *testing.T) {
	parser := NewParser(2)
	pz := NewPacketizer(1, 1, 2, 4)
	pkt, _ := pz.Packetize(make([]int32, 8))
	raw, _ := pkt.Marshal()

	// Drop one payload byte -> no longer 16-bit stereo aligned.
	bad := append([]byte(nil), raw...)
	bad = bad[:len(bad)-1]
	_, err := parser.Parse(bad, 0)
	var ce *clock.Error
	if !errors.As(err, &ce) || ce.Kind != clock.KindBadBlock {
		t.Fatalf("err=%v, want bad_block", err)
	}

	// Synthesize a packet whose payload exceeds the 64 KiB block cap.
	hdr2, _ := pkt.Header.Marshal()
	oversize := make([]byte, 0, len(hdr2)+clock.MaxBlockBytes+2)
	oversize = append(oversize, hdr2...)
	oversize = append(oversize, make([]byte, clock.MaxBlockBytes+2)...)
	_, err = parser.Parse(oversize, 0)
	if !errors.As(err, &ce) || ce.Kind != clock.KindBadBlock {
		t.Fatalf("err=%v, want bad_block for oversized block", err)
	}
}
