package gen

import (
	"bytes"
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/frame"
	"mediascan/internal/stream"
)

func params() FrameParams {
	return FrameParams{Seed: 73, Frames: 120, FPS: 30, GOP: 60, TargetKbps: 900, ClockRate: 90000}
}

func TestFramesDeterministicAndValid(t *testing.T) {
	a, err := Frames(params())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Frames(params())
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 120 {
		t.Fatalf("frames = %d", len(a))
	}
	for i := range a {
		if a[i].Entry.SHA256 != b[i].Entry.SHA256 || !bytes.Equal(a[i].Payload, b[i].Payload) {
			t.Fatalf("frame %d differs across runs", i)
		}
		if a[i].Entry.Size > frame.MaxFrameSize || a[i].Entry.Size <= 0 {
			t.Fatalf("frame %d size out of bounds", i)
		}
		// keyframes on the GOP grid
		wantKey := i%60 == 0
		if a[i].Entry.Keyframe != wantKey {
			t.Fatalf("frame %d keyframe = %v", i, a[i].Entry.Keyframe)
		}
		// verify payload really matches the recorded hash
		if f := a[i].Entry.Verify(a[i].Payload); !frame.IsZero(f) {
			t.Fatalf("frame %d hash mismatch on generation", i)
		}
	}
}

func TestSeedChangesOutput(t *testing.T) {
	p1, p2 := params(), params()
	p2.Seed = 74
	a, _ := Frames(p1)
	b, _ := Frames(p2)
	same := true
	for i := range a {
		if a[i].Entry.SHA256 != b[i].Entry.SHA256 {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds must change payloads")
	}
}

func TestRTPTraceImpairments(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	np := DefaultNetParams(73)
	pkts, err := RTPTrace(clk, 120, 1000, 0, np)
	if err != nil {
		t.Fatal(err)
	}
	// burst 24..27 removed => four sequence gaps, plus one duplicate packet
	seqs := map[uint16]int{}
	for _, p := range pkts {
		seqs[p.SequenceNumber]++
	}
	var dup int
	for _, n := range seqs {
		if n > 1 {
			dup++
		}
	}
	if dup != 1 {
		t.Fatalf("duplicates = %d, want 1", dup)
	}
	// seq for frames 24..27 must be absent while neighbours exist
	if seqs[1000+24] != 0 || seqs[1000+27] != 0 {
		t.Fatal("burst loss packets still present")
	}
	if seqs[1000+23] == 0 || seqs[1000+28] == 0 {
		t.Fatal("burst neighbours missing")
	}
	// reorder: arrival of frame 13 before frame 12
	order := make([]int, len(pkts))
	for i, p := range pkts {
		order[i] = int(p.Payload[0])
	}
	f12, f13 := indexOf(order, 12), indexOf(order, 13)
	if f12 < 0 || f13 < 0 || f13 >= f12 {
		t.Fatalf("reorder not reflected in arrival order: %d before %d", f13, f12)
	}
}

func TestRTPTraceTimestampWrap(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	np := DefaultNetParams(1)
	// start near the 32-bit boundary so the trace wraps
	pkts, err := RTPTrace(clk, 120, 0xFFFF, 0xFFFFFFF0, np)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) == 0 || pkts[0].Timestamp != 0xFFFFFFF0 {
		t.Fatal("wrapped trace did not start at boundary")
	}
	// a wrapped timestamp must appear
	wrapped := false
	for _, p := range pkts {
		if p.Timestamp < 0xFFFFFFF0 {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("expected a wrapped RTP timestamp")
	}
}

func TestBandwidthTrace(t *testing.T) {
	clk, _ := clock.New(90000, 30)
	a, err := Bandwidth(clk, 120, 900, 15, 73)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Bandwidth(clk, 120, 900, 15, 73)
	if len(a) != 8 || len(a) != len(b) {
		t.Fatalf("bandwidth samples = %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] || a[i].Bandwidth <= 0 {
			t.Fatal("bandwidth trace not deterministic or invalid")
		}
	}
}

func TestCorruptChecksumPayload(t *testing.T) {
	g, _ := Frames(params())
	original := append([]byte(nil), g[3].Payload...)
	if err := CorruptChecksumPayload(g[3].Payload, 3); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, g[3].Payload) {
		t.Fatal("payload was not modified")
	}
	if f := g[3].Entry.Verify(g[3].Payload); frame.IsZero(f) {
		t.Fatal("corrupted payload unexpectedly passed checksum")
	}
}

func TestTruncatedMP4Detected(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	corrupt, err := TruncatedMP4(payload, 40)
	if err != nil {
		t.Fatal(err)
	}
	a, err := stream.Scan(bytes.NewReader(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range a.Faults {
		if string(f.Cat) == "truncated_box" {
			found = true
		}
	}
	if !found {
		t.Fatal("truncated MP4 not detected by scanner")
	}
}

func indexOf(xs []int, want int) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
