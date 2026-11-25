package ts

import (
	"testing"

	"cinepipe/internal/clock"
	"cinepipe/internal/fault"
)

func TestParsePCRRoundTrip(t *testing.T) {
	want := PCR{Base: 123456789, Ext: 250}
	raw := BuildPCRBytes(want)
	got, ok := parsePCRBytes(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	if got.Base != want.Base || got.Ext != want.Ext {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

// parsePCRBytes is a tiny helper for the test, mirroring ParsePCR's byte
// layout through an AdaptationField wrapper.
func parsePCRBytes(b []byte) (PCR, bool) {
	af := &AdaptationField{HasPCR: true, Raw: b}
	return ParsePCR(af)
}

func TestParsePCRRejectsBadExtension(t *testing.T) {
	raw := BuildPCRBytes(PCR{Base: 100, Ext: 400}) // out of range
	if _, ok := parsePCRBytes(raw); ok {
		t.Fatal("should reject extension 400")
	}
	af := &AdaptationField{HasPCR: false, Raw: make([]byte, 6)}
	if _, ok := ParsePCR(af); ok {
		t.Fatal("should reject missing PCR flag")
	}
}

func TestPCRTrackerWrap(t *testing.T) {
	tr := NewPCRTracker()
	nearTop := clock.PCRModulus - 5000
	p0 := &Packet{Offset: 0}
	abs0, wrapped, errs := tr.Observe(p0, PCR{Base: nearTop}, false)
	if wrapped || len(errs) != 0 {
		t.Fatalf("first: %v %v", wrapped, errs)
	}
	// Cross the modulus: base wraps to 3000.
	abs1, wrapped, errs := tr.Observe(&Packet{Offset: 188}, PCR{Base: 3000}, false)
	if !wrapped || len(errs) != 0 {
		t.Fatalf("wrap not detected: wrapped=%v errs=%v", wrapped, errs)
	}
	if abs1-abs0 != 8000 {
		t.Fatalf("delta %d want 8000", abs1-abs0)
	}
	if tr.Origin() != abs0 {
		t.Fatalf("origin %d", tr.Origin())
	}
	if tr.Rel(abs1) != 8000 {
		t.Fatalf("relative %d", tr.Rel(abs1))
	}
}

func TestPCRTrackerRewindIsError(t *testing.T) {
	tr := NewPCRTracker()
	tr.Observe(&Packet{Offset: 0}, PCR{Base: 100000}, false)
	// A large backwards step without a discontinuity indicator.
	_, _, errs := tr.Observe(&Packet{Offset: 188, PID: 0x100}, PCR{Base: 50000}, false)
	if len(errs) != 1 || errs[0].Kind != fault.KindContinuity {
		t.Fatalf("expected rewind error: %v", errs)
	}
	// With a discontinuity indicator the same step is legitimate.
	tr2 := NewPCRTracker()
	tr2.Observe(&Packet{Offset: 0}, PCR{Base: 100000}, false)
	_, _, errs = tr2.Observe(&Packet{Offset: 188}, PCR{Base: 50000}, true)
	if len(errs) != 0 {
		t.Fatalf("discontinuity should be accepted: %v", errs)
	}
}

func buildPESHeader(streamID byte, pts, dts uint64, hasDTS bool) []byte {
	h := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x80, 0x05}
	if hasDTS {
		h[7] = 0xC0
		h[8] = 0x0A
	}
	h = append(h, ptsBytes(pts)...)
	if hasDTS {
		h = append(h, ptsBytes(dts)...)
	}
	return h
}

// ptsBytes renders a 33-bit timestamp into the marker-bit layout: b[0] holds
// bits 32..30, b[2] bits 21..15, b[4] bits 6..0.
func ptsBytes(v uint64) []byte {
	return []byte{
		0x20 | byte((v>>30)&0x07)<<1 | 0x01,
		byte(v >> 22),
		byte((v>>15)&0x7F)<<1 | 0x01,
		byte(v >> 7),
		byte(v&0x7F)<<1 | 0x01,
	}
}

func TestParsePESHeaderPTS(t *testing.T) {
	want := uint64(0x1_2345_6789) & (1<<33 - 1)
	h := buildPESHeader(0xE0, want, 0, false)
	ts, ok, reason := ParsePESHeader(h)
	if !ok {
		t.Fatalf("parse failed: %s", reason)
	}
	if !ts.HasPTS || ts.PTS != want {
		t.Fatalf("pts %x want %x (%+v)", ts.PTS, want, ts)
	}
	if ts.DataStart != 9+5 {
		t.Fatalf("data start %d", ts.DataStart)
	}
}

func TestParsePESHeaderPTSAndDTS(t *testing.T) {
	pts := uint64(500000)
	dts := uint64(400000)
	h := buildPESHeader(0xE0, pts, dts, true)
	ts, ok, reason := ParsePESHeader(h)
	if !ok {
		t.Fatalf("parse failed: %s", reason)
	}
	if !ts.HasDTS || ts.PTS != pts || ts.DTS != dts {
		t.Fatalf("ts %+v", ts)
	}
	if ts.DataStart != 9+10 {
		t.Fatalf("data start %d", ts.DataStart)
	}
}

func TestParsePESHeaderRejectsTruncation(t *testing.T) {
	if _, ok, reason := ParsePESHeader([]byte{0, 0, 1, 0xE0}); ok || reason == "" {
		t.Fatal("short header should fail with reason")
	}
	b := buildPESHeader(0xE0, 1, 0, false)[:10]
	if _, ok, reason := ParsePESHeader(b); ok || reason == "" {
		t.Fatal("truncated PTS should fail")
	}
	if _, ok, reason := ParsePESHeader([]byte{0xDE, 0xAD, 0xBE}); ok {
		t.Fatalf("bad start code accepted: %s", reason)
	}
}

func TestTimestampSetExpand(t *testing.T) {
	set := NewTimestampSet()
	// Start just before the modulus so the next small timestamp crosses it.
	first := PESTimestamp{HasPTS: true, PTS: clock.PTSModulus - 1000}
	abs, _ := set.Observe(first)
	if abs != int64(clock.PTSModulus-1000) || set.Origin() != abs {
		t.Fatalf("abs=%d origin=%d", abs, set.Origin())
	}
	ts2 := PESTimestamp{HasPTS: true, PTS: 200}
	abs2, wrapped := set.Observe(ts2)
	if !wrapped {
		t.Fatal("expected wrap")
	}
	if got, want := set.Rel(abs2), int64(1200); got != want {
		t.Fatalf("rel %d want %d", got, want)
	}
}

func TestExtendPTSEpoch(t *testing.T) {
	// A raw PTS near the top of the modulus, referenced one epoch in.
	raw := clock.PTSModulus - 1000
	ref := int64(2*clock.PTSModulus) + 500
	got := ExtendPTS(raw, ref, 0)
	if want := int64(2*clock.PTSModulus) - 1000; got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}
