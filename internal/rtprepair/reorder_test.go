package rtprepair_test

import (
	"bytes"
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/rtprepair"
	"mediasync/internal/testdata"
)

func decodeAll(t *testing.T, fixture testdata.RTPFixture) []*rtprepair.Packet {
	t.Helper()
	out, _ := rtprepair.Simulate(fixture.Packets, rtprepair.DefaultOptions(), "cam.rtp")
	return out
}

func seqs(packets []*rtprepair.Packet) []uint32 {
	out := make([]uint32, len(packets))
	for i, p := range packets {
		out[i] = p.Seq
	}
	return out
}

func TestReorderAndDedup(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 120, PayloadSize: 64, SSRC: 7,
		StartSeq: 1000, DuplicateEvery: 13, ReorderEvery: 9})
	emitted, r := rtprepair.Simulate(fx.Packets, rtprepair.DefaultOptions(), "cam.rtp")
	st := r.Stats()
	if st.DuplicatePackets < fx.Duplicates {
		t.Errorf("duplicates %d, fixture injected %d", st.DuplicatePackets, fx.Duplicates)
	}
	if st.ReorderedPackets < fx.Reordered {
		t.Errorf("reordered %d, fixture injected %d", st.ReorderedPackets, fx.Reordered)
	}
	if st.LostPackets != 0 {
		t.Errorf("lost %d packets with no loss injected", st.LostPackets)
	}
	want := len(fx.Packets) - fx.Duplicates
	if len(emitted) != want {
		t.Errorf("emitted %d packets, want %d", len(emitted), want)
	}
	// Emission is strictly ordered even though arrival was not.
	s := seqs(emitted)
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			t.Fatalf("output not contiguous at %d: %d -> %d", i, s[i-1], s[i])
		}
	}
}

func TestBurstLossIsBounded(t *testing.T) {
	plan := testdata.RTPPlan{Packets: 200, PayloadSize: 96, SSRC: 9, StartSeq: 60000,
		BurstLoss: 4, BurstCount: 3, ReorderEvery: 7, DuplicateEvery: 19}
	fx := testdata.GenerateRTP(plan)
	emitted, r := rtprepair.Simulate(fx.Packets, rtprepair.DefaultOptions(), "cam.rtp")
	st := r.Stats()
	if st.LostPackets != fx.BurstLost {
		t.Errorf("lost %d packets, fixture dropped %d", st.LostPackets, fx.BurstLost)
	}
	if st.BurstLostPackets != fx.BurstLost {
		t.Errorf("burst_lost %d, want %d (all losses are bursts of 4)", st.BurstLostPackets, fx.BurstLost)
	}
	if len(emitted)+st.LostPackets+st.DuplicatePackets < st.Received {
		t.Errorf("accounting mismatch: emitted=%d lost=%d dup=%d received=%d",
			len(emitted), st.LostPackets, st.DuplicatePackets, st.Received)
	}
	// Every emitted payload must be well-formed and round-trip to its raw bytes.
	for i, p := range emitted {
		raw, err := p.Marshal()
		if err != nil {
			t.Fatalf("packet %d remarshal: %v", i, err)
		}
		if !bytes.Equal(raw, p.Raw) {
			t.Fatalf("packet %d roundtrip mismatch", i)
		}
	}
	if st.Consistency() < 0.99 {
		t.Errorf("roundtrip consistency %.4f below 0.99", st.Consistency())
	}
}

func TestSequenceWrapAround(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 200, PayloadSize: 32, SSRC: 3, StartSeq: 65500})
	emitted, r := rtprepair.Simulate(fx.Packets, rtprepair.DefaultOptions(), "wrap.rtp")
	if len(emitted) != 200 {
		t.Fatalf("emitted %d packets across wrap, want 200", len(emitted))
	}
	prev := emitted[0].Seq
	for i := 1; i < len(emitted); i++ {
		if emitted[i].Seq != prev+1 {
			t.Fatalf("wrap not unwrapped at %d: %d -> %d", i, prev, emitted[i].Seq)
		}
		prev = emitted[i].Seq
	}
	if r.Stats().LostPackets != 0 {
		t.Errorf("wrap caused %d false losses", r.Stats().LostPackets)
	}
}

func TestWindowExceededForcesFlush(t *testing.T) {
	r := rtprepair.New(rtprepair.Options{Window: 3}, "w.rtp")
	push := func(seq uint16) []*rtprepair.Packet {
		p, err := r.Parse(rtprepair.Encode(seq, uint32(seq)*3000, 1, []byte("x")), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return r.Push(p)
	}
	for _, seq := range []uint16{1, 2, 3} {
		push(seq)
	}
	// Jump past the window: 4..7 are declared lost and 8 is emitted.
	out := push(8)
	if len(out) != 1 || out[0].Seq != 8 {
		t.Fatalf("expected packet 8 after forced flush, got %v", seqs(out))
	}
	st := r.Stats()
	if st.LostPackets != 4 {
		t.Errorf("lost = %d, want 4", st.LostPackets)
	}
	if st.BurstLostPackets != 4 {
		t.Errorf("burst_lost = %d, want 4", st.BurstLostPackets)
	}
	if st.ForcedFlushes == 0 {
		t.Error("forced flush not recorded")
	}
	// The flush path must leave no buffered packet behind.
	if rest := r.Flush(); len(rest) != 0 {
		t.Errorf("flush returned %d packets after forced flush", len(rest))
	}
}

func TestNoDeadlockOnPermanentHole(t *testing.T) {
	r := rtprepair.New(rtprepair.Options{Window: 8, MaxBuffered: 4}, "hole.rtp")
	// Packet 0 is delivered, packet 1 is missing forever.
	first, _ := r.Parse(rtprepair.Encode(0, 0, 1, []byte("y")), 0, 0)
	var emitted = len(r.Push(first))
	for seq := uint16(2); seq < 20; seq++ {
		p, _ := r.Parse(rtprepair.Encode(seq, uint32(seq)*3000, 1, []byte("y")), 0, 0)
		emitted += len(r.Push(p))
	}
	emitted += len(r.Flush())
	st := r.Stats()
	if st.LostPackets != 1 {
		t.Errorf("lost = %d, want exactly the one permanent hole", st.LostPackets)
	}
	if emitted < 17 {
		t.Errorf("emitted %d packets, stream stalled", emitted)
	}
}

func TestMalformedPacketIsRecoverable(t *testing.T) {
	r := rtprepair.New(rtprepair.DefaultOptions(), "bad.rtp")
	if _, err := r.Parse([]byte{0x00}, 0, 0); err == nil {
		t.Fatal("truncated RTP header accepted")
	}
	if _, err := r.Parse(bytes.Repeat([]byte{0xFF}, rtprepair.DefaultMaxPacket+1), 0, 0); err == nil {
		t.Fatal("oversized packet accepted")
	}
	if got := r.Stats().MalformedPackets; got != 2 {
		t.Errorf("malformed = %d, want 2", got)
	}
	issues := r.Issues()
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].Stage != media.StageRTP || issues[0].Input != media.InputRTP {
		t.Errorf("issue metadata = %+v", issues[0])
	}
}

func TestLateDuplicateAfterEmission(t *testing.T) {
	r := rtprepair.New(rtprepair.DefaultOptions(), "dup.rtp")
	send := func(seq uint16) {
		p, err := r.Parse(rtprepair.Encode(seq, uint32(seq)*3000, 1, []byte("z")), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		r.Push(p)
	}
	send(1)
	send(2)
	send(1) // arrives after being delivered
	if got := r.Stats().DuplicatePackets; got != 1 {
		t.Errorf("duplicate = %d, want 1", got)
	}
	if got := r.Stats().Emitted; got != 2 {
		t.Errorf("emitted = %d, want 2", got)
	}
}

func TestEmptyStream(t *testing.T) {
	emitted, r := rtprepair.Simulate(nil, rtprepair.DefaultOptions(), "empty.rtp")
	if len(emitted) != 0 {
		t.Errorf("emitted %d packets from an empty stream", len(emitted))
	}
	if r.Stats().Consistency() != 1 {
		t.Errorf("empty consistency = %v, want 1", r.Stats().Consistency())
	}
}
