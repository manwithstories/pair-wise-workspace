package ingest

import (
	"bytes"
	"io"
	"testing"

	"github.com/pion/rtp"

	"mediameld/internal/fixture"
	"mediameld/internal/model"
)

func generateRTP(t *testing.T) []byte {
	t.Helper()
	b, err := fixture.Generate(t.TempDir(), fixture.Options{
		Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := openFile(t, b.RTPStream)
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDemuxRTPCountsAndDuplicates(t *testing.T) {
	raw := generateRTP(t)
	plan := fixture.PlanDelivery(72, 12)
	res, err := DemuxRTP(bytes.NewReader(raw), "cam-video")
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	// 72 source frames, one duplicate -> 73 packets.
	want := 72
	if plan.Duplicate >= 0 {
		want++
	}
	if plan.Missing >= 0 {
		want--
	}
	if len(res.Packets) != want {
		t.Fatalf("packets=%d want %d", len(res.Packets), want)
	}
	seen := map[uint16]int{}
	for _, p := range res.Packets {
		seen[p.Seq]++
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups++
		}
	}
	if dups != 1 {
		t.Fatalf("duplicate sequences=%d want 1", dups)
	}
	// At least one timestamp must have wrapped to a value below its
	// predecessor in sequence order.
	wrapped := false
	var prev uint32
	first := true
	for _, p := range res.Packets {
		if !first && p.Timestamp < prev {
			wrapped = true
		}
		prev = p.Timestamp
		first = false
	}
	if !wrapped {
		t.Fatal("no 32-bit wrap observed in fixture timestamps")
	}
}

func TestDemuxRTPBadFraming(t *testing.T) {
	bad := []byte{0x00, 0x00, 0x00, 0x40, 0x80}
	_, err := DemuxRTP(bytes.NewReader(bad), "x")
	if err == nil {
		t.Fatal("overrun framing accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeRTPPacket {
		t.Fatalf("code=%v", code)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit=%d want 2", model.ExitCode(err))
	}
}

func TestDemuxRTPEmpty(t *testing.T) {
	_, err := DemuxRTP(bytes.NewReader(nil), "x")
	if err == nil {
		t.Fatal("empty stream accepted")
	}
}

func TestCalibrateRTPUndoesWrapAndDrift(t *testing.T) {
	raw := generateRTP(t)
	res, err := DemuxRTP(bytes.NewReader(raw), "cam-video")
	if err != nil {
		t.Fatal(err)
	}
	cal := CalibrateRTP(res, 85)
	if !cal.Monotonic {
		t.Fatal("calibrated timestamps not monotonic after unwrap")
	}
	if cal.NS[0] != 0 {
		t.Fatalf("frame zero reference=%d want 0", cal.NS[0])
	}
	// All 72 source frames arrived, so every slot must be populated.
	populated := 0
	for _, ns := range cal.NS {
		if ns >= 0 {
			populated++
		}
	}
	if populated != 72 {
		t.Fatalf("calibrated frames=%d want 72", populated)
	}
	// A frame that is late in arrival order must still have a presentation
	// time between its neighbours.
	for i := 1; i < len(cal.NS); i++ {
		if cal.NS[i] <= cal.NS[i-1] {
			t.Fatalf("frame %d not strictly after frame %d", i, i-1)
		}
	}
}

func TestCalibrateRTPAcceptsLateArrival(t *testing.T) {
	// Receive order: seq 1, then seq 3, then the late seq 2.
	raw := appendRTPPacket(t, nil, 1, 1000)
	raw = appendRTPPacket(t, raw, 3, 3000)
	raw = appendRTPPacket(t, raw, 2, 2000)
	res, err := DemuxRTP(bytes.NewReader(raw), "cam")
	if err != nil {
		t.Fatal(err)
	}
	cal := CalibrateRTP(res, 0)
	if !cal.Monotonic {
		t.Fatal("reordered arrival broke monotonicity")
	}
	// Arrival order is preserved verbatim for late detection, with frame
	// indexes derived from the base sequence number.
	want := []int{0, 2, 1}
	for i, v := range want {
		if cal.Order[i] != v {
			t.Fatalf("order=%v want %v", cal.Order, want)
		}
	}
}

func appendRTPPacket(t *testing.T, dst []byte, seq uint16, ts uint32) []byte {
	t.Helper()
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: []byte("F0payload"),
	}
	raw, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var lenBuf [4]byte
	lenBuf[0] = byte(len(raw) >> 24)
	lenBuf[1] = byte(len(raw) >> 16)
	lenBuf[2] = byte(len(raw) >> 8)
	lenBuf[3] = byte(len(raw))
	dst = append(dst, lenBuf[:]...)
	return append(dst, raw...)
}
