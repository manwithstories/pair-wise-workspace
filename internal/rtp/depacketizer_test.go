package rtp

import (
	"testing"

	pionrtp "github.com/pion/rtp"
)

func mkPacket(seq uint16, ts uint32, marker bool, payload byte) []byte {
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: 1, Marker: marker,
		},
		Payload: []byte{payload, payload + 1, payload + 2},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return raw
}

func pushAll(t *testing.T, d *Depacketizer, raws [][]byte) int {
	t.Helper()
	frames := 0
	for _, raw := range raws {
		out, err := d.Push(raw)
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		frames += len(out)
	}
	return frames
}

func TestReorderDuplicateAndFrame(t *testing.T) {
	d := NewDepacketizer(8)
	// seqs 10..15, two packets per frame (marker on even index), seq 12 late.
	var raws [][]byte
	seq := uint16(10)
	ts := uint32(100)
	for i := 0; i < 6; i++ {
		marker := i%2 == 1
		raws = append(raws, mkPacket(seq+uint16(i), ts+uint32(i/2)*10, marker, byte(i)))
	}
	// reorder: swap positions 2 and 3 (seq 12 after 13)
	raws[2], raws[3] = raws[3], raws[2]
	// duplicate seq 10 again at the end
	raws = append(raws, mkPacket(10, 100, false, 0))

	frames := pushAll(t, d, raws)
	if fr := d.Flush(); fr != nil {
		frames++
	}
	if frames != 3 {
		t.Fatalf("frames = %d, want 3", frames)
	}
	st := d.Stats()
	if st.Received != 7 || st.Duplicates != 1 {
		t.Fatalf("received=%d dup=%d", st.Received, st.Duplicates)
	}
	// Adjacent swap: seq 12 arrives after higher seq 13, so it is reordered.
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d, want 1 for gapless swap", st.Reordered)
	}
	if st.MaxReorderDepth != 1 {
		t.Fatalf("depth=%d, want 1", st.MaxReorderDepth)
	}
	if st.Frames != 3 || st.Bytes != 18 {
		t.Fatalf("frames=%d bytes=%d", st.Frames, st.Bytes)
	}
	if st.FirstSeq != 10 || st.LastSeq != 15 {
		t.Fatalf("bounds %d..%d want 10..15", st.FirstSeq, st.LastSeq)
	}
}

func TestGapsAndWindow(t *testing.T) {
	d := NewDepacketizer(4)
	// deliver 0, then jump to 10: window boundary closes gap 1..5.
	var raws [][]byte
	raws = append(raws, mkPacket(0, 0, false, 1))
	for seq := uint16(10); seq <= 14; seq++ {
		raws = append(raws, mkPacket(seq, uint32(seq)*10, seq == 14, byte(seq)))
	}
	pushAll(t, d, raws)
	st, gaps := d.Finalize()
	if st.GapSegments != 1 {
		t.Fatalf("segments=%d gaps=%+v", st.GapSegments, gaps)
	}
	if gaps[0].First != 1 || gaps[0].Count != 9 {
		t.Fatalf("gap = %+v, want 1..9 (9 packets)", gaps[0])
	}
	if st.GapPackets != 9 {
		t.Fatalf("gap packets = %d", st.GapPackets)
	}
}

func TestWrapAround(t *testing.T) {
	d := NewDepacketizer(8)
	var raws [][]byte
	// 65533, 65534, 65535, 0, 1, 2 with markers every two packets.
	seqs := []uint16{65533, 65534, 65535, 0, 1, 2}
	for i, seq := range seqs {
		raws = append(raws, mkPacket(seq, uint32(i/2)*100, i%2 == 1, byte(i)))
	}
	// deliver out of order: 65535 before 65534 to test wrap reordering.
	raws[1], raws[2] = raws[2], raws[1]
	frames := pushAll(t, d, raws)
	if fr := d.Flush(); fr != nil {
		frames++
	}
	if frames != 3 {
		t.Fatalf("frames across wrap = %d, want 3", frames)
	}
	st, gaps := d.Finalize()
	if len(gaps) != 0 || st.GapPackets != 0 {
		t.Fatalf("unexpected gaps across wrap: %+v", gaps)
	}
	if st.Reordered != 1 {
		t.Fatalf("reordered = %d, want 1 for gapless swap", st.Reordered)
	}
	if st.MaxReorderDepth != 1 {
		t.Fatalf("depth = %d, want 1", st.MaxReorderDepth)
	}
	if st.FirstSeq != 65533 || st.LastSeq != 2 {
		t.Fatalf("bounds across wrap: %d..%d", st.FirstSeq, st.LastSeq)
	}
}

func TestLatePacketAfterGapCountsReorder(t *testing.T) {
	d := NewDepacketizer(4)
	var raws [][]byte
	// seq 0 then a gap (1 missing), then 2..6 so the window skips seq 1, then
	// the late seq 1 arrives below the advanced frontier.
	raws = append(raws, mkPacket(0, 0, false, 0))
	for seq := uint16(2); seq <= 6; seq++ {
		raws = append(raws, mkPacket(seq, uint32(seq)*10, seq == 6, byte(seq)))
	}
	raws = append(raws, mkPacket(1, 10, false, 1)) // late fill
	pushAll(t, d, raws)
	st, _ := d.Finalize()
	if st.Reordered != 1 {
		t.Fatalf("reordered=%d, want 1 late packet", st.Reordered)
	}
	// The window expelled seq 1 as a permanent gap before the late packet
	// arrived; the late packet still counts as one reordered packet.
	if st.GapSegments != 1 || st.GapPackets != 1 {
		t.Fatalf("expelled gap should remain recorded, got %+v", d.Gaps())
	}
}
