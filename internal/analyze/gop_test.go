package analyze

import (
	"testing"

	"tscheck/internal/model"
	"tscheck/internal/ts"
)

func TestContainsIDR(t *testing.T) {
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x99}
	if !ContainsIDR(idr) {
		t.Fatal("IDR not detected")
	}
	nonIDR := []byte{0x00, 0x00, 0x01, 0x41, 0x9A}
	if ContainsIDR(nonIDR) {
		t.Fatal("non-IDR misdetected")
	}
	spsThenIDR := []byte{0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x00, 0x00, 0x01, 0x65}
	if !ContainsIDR(spsThenIDR) {
		t.Fatal("IDR after SPS not detected")
	}
	if ContainsIDR([]byte{0x00, 0x00}) {
		t.Fatal("empty stream misdetected")
	}
}

// mkPESPayload builds a PES payload (header + ES) with a PTS.
func mkPESPayload(pts int64, es []byte) []byte {
	hdrlen := 5
	declared := 3 + hdrlen + len(es)
	p := []byte{0x00, 0x00, 0x01, 0xE0, byte(declared >> 8), byte(declared),
		0x80, 0x80, byte(hdrlen)}
	p = append(p,
		byte(0x02<<4|((pts>>30)&0x07)<<1|1),
		byte(pts>>22),
		byte(((pts>>15)&0x7F)<<1|1),
		byte(pts>>7),
		byte((pts&0x7F)<<1|1),
	)
	return append(p, es...)
}

func TestPESAssemblerComplete(t *testing.T) {
	a := NewPESAssembler(0x100)
	payload := mkPESPayload(126000, []byte{0x00, 0x00, 0x00, 0x01, 0x65})
	pes, fault := a.Feed(ts.PacketInfo{PID: 0x100, PUSI: true, Payload: payload, Offset: 188})
	if fault != nil {
		t.Fatalf("fault = %v", fault)
	}
	if pes == nil || !pes.Complete || !pes.HasPTS || pes.PTSRaw != 126000 {
		t.Fatalf("pes = %+v", pes)
	}
	if !ContainsIDR(pes.ES) {
		t.Fatalf("ES = %x", pes.ES)
	}
	if f := a.Flush(); f != nil {
		t.Fatalf("flush fault = %v", f)
	}
}

func TestPESAssemblerIncompleteBoundary(t *testing.T) {
	a := NewPESAssembler(0x100)
	full := mkPESPayload(1000, []byte{1, 2, 3, 4})
	// Feed only the 14-byte header of the declared PES, then start the
	// next unit before any ES byte arrives.
	if _, f := a.Feed(ts.PacketInfo{PID: 0x100, PUSI: true, Payload: full[:14]}); f != nil {
		t.Fatalf("first feed fault = %v", f)
	}
	_, f := a.Feed(ts.PacketInfo{PID: 0x100, PUSI: true, Payload: full, Offset: 376})
	if f == nil || f.Kind != model.FaultIncompletePES {
		t.Fatalf("expected incomplete PES fault, got %v", f)
	}
}

func TestPESAssemblerRejectsOversize(t *testing.T) {
	a := NewPESAssembler(0x100)
	p := []byte{0x00, 0x00, 0x01, 0xE0, 0xFF, 0xFF, 0x80, 0x00, 0x00}
	_, f := a.Feed(ts.PacketInfo{PID: 0x100, PUSI: true, Payload: p})
	// 0xFFFF < MaxPESBytes: accepted. Now force beyond the limit.
	if f != nil {
		t.Fatalf("unexpected fault = %v", f)
	}
	big := make([]byte, MaxPESBytes+1)
	_, f = a.Feed(ts.PacketInfo{PID: 0x100, Payload: big})
	if f == nil || f.Kind != model.FaultOversizePES {
		t.Fatalf("expected oversize fault, got %v", f)
	}
}

func TestGOPTrackerQuantilesAndUnanchored(t *testing.T) {
	g := NewGOPTracker()
	// 10 frames, 900k ticks apart (30 fps), keyframes at 0 and 8.1M.
	for i := 0; i < 10; i++ {
		g.AddFrame(model.Timestamp{Ticks: model.Ticks(i * 900000), Anchored: true}, i%5 == 0)
	}
	// Unanchored frame must be ignored entirely.
	g.AddFrame(model.Timestamp{Ticks: 999, Anchored: false}, true)
	st := g.Stats()
	if st.Frames != 10 || st.Keyframes != 2 {
		t.Fatalf("stats = %+v", st)
	}
	if st.IntervalP50 != 900000 || st.IntervalP90 != 900000 {
		t.Fatalf("quantiles = %+v", st)
	}
	if len(st.FrameDeltas) != 9 {
		t.Fatalf("deltas = %d", len(st.FrameDeltas))
	}
}
