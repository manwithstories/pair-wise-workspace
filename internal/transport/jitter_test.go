package transport_test

import (
	"bytes"
	"errors"
	"testing"

	"avcheck/internal/media"
	"avcheck/internal/transport"
)

func mustBuffer(t *testing.T, cfg transport.Config, faults *media.Faults) *transport.Buffer {
	t.Helper()
	b, err := transport.NewBuffer(1, cfg, faults)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pkt(seq uint32, payload string, arrival int64) transport.Packet {
	return transport.Packet{Stream: 1, Seq: seq, Timestamp: seq, Payload: []byte(payload), ArrivalNs: arrival}
}

// TestReorderAndDuplicate pushes an out-of-order stream with a duplicate and
// requires the buffer to emit strictly increasing sequence numbers once.
func TestReorderAndDuplicate(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.LatencyNs = 10
	faults := &media.Faults{}
	b := mustBuffer(t, cfg, faults)
	order := []uint32{1, 0, 3, 2, 2, 4}
	for i, seq := range order {
		if err := b.Push(pkt(seq, "x", int64(i))); err != nil {
			t.Fatalf("push %d: %v", seq, err)
		}
	}
	out := b.PopReady(1 << 30)
	out = append(out, b.Flush()...)
	var got []uint32
	for _, p := range out {
		got = append(got, p.Seq)
	}
	want := []uint32{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("emitted %v, want %v", got, want)
		}
	}
	st := b.Stats()
	if st.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", st.Duplicates)
	}
	if st.Reordered == 0 {
		t.Fatal("reordering was not counted")
	}
	if !faults.Has(media.CodeDuplicatePacket) {
		t.Fatalf("faults = %v, want duplicate_packet", faults)
	}
	if lvl, _ := faults.First(media.CodeDuplicatePacket); lvl.Level != media.LevelRecovered {
		t.Fatalf("duplicate level = %v, want recovered", lvl.Level)
	}
}

// TestSequenceWrap proves sequence comparison is wrap-safe.
func TestSequenceWrap(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.LatencyNs = 0
	b := mustBuffer(t, cfg, &media.Faults{})
	seqs := []uint32{0xFFFFFFFE, 0xFFFFFFFF, 0, 1, 2}
	for i, s := range seqs {
		if err := b.Push(pkt(s, "w", int64(i))); err != nil {
			t.Fatalf("push %d: %v", s, err)
		}
	}
	out := b.PopReady(100)
	if len(out) != len(seqs) {
		t.Fatalf("emitted %d packets, want %d", len(out), len(seqs))
	}
	for i := range out {
		if out[i].Seq != seqs[i] {
			t.Fatalf("packet %d seq = %d, want %d", i, out[i].Seq, seqs[i])
		}
	}
}

// TestByteCapBackpressure fills the queue past its cap and requires
// ErrBackpressure plus an explicit slow-consumer fault, and no overshoot.
func TestByteCapBackpressure(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.MaxBufferBytes = 100
	cfg.LatencyNs = 1e9
	faults := &media.Faults{}
	b := mustBuffer(t, cfg, faults)
	var err error
	for i := 0; i < 10; i++ {
		err = b.Push(pkt(uint32(i), string(bytes.Repeat([]byte("z"), 30)), int64(i)))
		if err != nil {
			break
		}
	}
	if !errors.Is(err, transport.ErrBackpressure) {
		t.Fatalf("err = %v, want ErrBackpressure", err)
	}
	if b.Bytes() > cfg.MaxBufferBytes {
		t.Fatalf("queued %d bytes, cap %d", b.Bytes(), cfg.MaxBufferBytes)
	}
	if !faults.Has(media.CodeSlowConsumer) {
		t.Fatalf("faults = %v, want slow_consumer", faults)
	}
}

// TestCloseReleasesQueue checks cancellation frees the queue and rejects
// later pushes.
func TestCloseReleasesQueue(t *testing.T) {
	b := mustBuffer(t, transport.DefaultConfig(), &media.Faults{})
	if err := b.Push(pkt(1, "abc", 0)); err != nil {
		t.Fatal(err)
	}
	if b.Bytes() == 0 {
		t.Fatal("expected queued bytes before close")
	}
	b.Close()
	if b.Bytes() != 0 {
		t.Fatalf("bytes after close = %d", b.Bytes())
	}
	if err := b.Push(pkt(2, "d", 0)); !errors.Is(err, transport.ErrCanceled) {
		t.Fatalf("push after close = %v, want ErrCanceled", err)
	}
	if got := b.PopReady(1 << 40); got != nil {
		t.Fatalf("pop after close returned %d packets", len(got))
	}
}

// TestGapIsExplicit requires a missing sequence to surface as a counted gap
// rather than a silently skipped packet.
func TestGapIsExplicit(t *testing.T) {
	cfg := transport.DefaultConfig()
	cfg.LatencyNs = 0
	faults := &media.Faults{}
	b := mustBuffer(t, cfg, faults)
	for _, s := range []uint32{0, 1, 4, 5} {
		if err := b.Push(pkt(s, "p", int64(s))); err != nil {
			t.Fatal(err)
		}
	}
	out := b.PopReady(1000)
	out = append(out, b.Flush()...)
	var got []uint32
	for _, p := range out {
		got = append(got, p.Seq)
	}
	want := []uint32{0, 1, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	st := b.Stats()
	if st.GapPackets != 2 || st.GapRuns != 1 {
		t.Fatalf("gaps packets=%d runs=%d, want 2/1", st.GapPackets, st.GapRuns)
	}
	if !faults.Has(media.CodeJitterOverflow) {
		t.Fatalf("faults = %v, want a jitter gap", faults)
	}
}

// TestSinglePacketXORRecovery loses one packet in each group and proves the
// recovered bytes equal the sender's payload exactly.
func TestSinglePacketXORRecovery(t *testing.T) {
	const group = 8
	const frames = 32
	faults := &media.Faults{}
	rep, err := transport.NewRepairer(1, group, faults)
	if err != nil {
		t.Fatal(err)
	}
	payloads := make([][]byte, frames)
	framesSeq := make([]transport.Frame, frames)
	for i := 0; i < frames; i++ {
		payloads[i] = []byte{byte(i), byte(i * 7), byte(255 - i), 0xAA}
		framesSeq[i] = transport.Frame{Track: 1, Seq: uint32(i), Ts: uint32(i * 100),
			Payload: payloads[i], SendNs: int64(i) * 1000}
	}
	res := transport.Impair(framesSeq, transport.LinkConfig{
		Seed: 1, GroupSize: group, DropSinglesEvery: 1,
	})
	if res.Dropped != frames/group {
		t.Fatalf("dropped = %d, want %d", res.Dropped, frames/group)
	}
	for _, d := range res.Media {
		p, err := transport.DecodeMedia(d)
		if err != nil {
			t.Fatal(err)
		}
		rep.Add(p)
	}
	sawRecovered := 0
	for _, d := range res.Parity {
		h, xor, err := transport.DecodeParity(d)
		if err != nil {
			t.Fatal(err)
		}
		if rep.FeedParity(h, xor) {
			sawRecovered++
		}
	}
	rep.Finish()
	if sawRecovered != frames/group {
		t.Fatalf("recovered groups = %d, want %d", sawRecovered, frames/group)
	}
	if rep.RecoveredPayloadPct() != 100 {
		t.Fatalf("recovered pct = %.1f, want 100", rep.RecoveredPayloadPct())
	}
	for _, seq := range rep.RecoveredSeqs() {
		got, ok := rep.Payload(seq)
		if !ok {
			t.Fatalf("payload for recovered seq %d missing", seq)
		}
		if !bytes.Equal(got, payloads[seq]) {
			t.Fatalf("recovered seq %d = %x, want %x", seq, got, payloads[seq])
		}
	}
}

// TestDoubleLossLeavesGap loses two packets in one group: the repairer must
// refuse to invent the second payload and record both as explicit gaps.
func TestDoubleLossLeavesGap(t *testing.T) {
	const group = 8
	const frames = 16
	faults := &media.Faults{}
	rep, err := transport.NewRepairer(1, group, faults)
	if err != nil {
		t.Fatal(err)
	}
	fr := make([]transport.Frame, frames)
	for i := 0; i < frames; i++ {
		fr[i] = transport.Frame{Track: 1, Seq: uint32(i), Ts: uint32(i * 10),
			Payload: bytes.Repeat([]byte{byte(i)}, 8), SendNs: int64(i)}
	}
	res := transport.Impair(fr, transport.LinkConfig{Seed: 2, GroupSize: group, DoubleLossGroup: 1})
	if res.DoubleDropped != 2 {
		t.Fatalf("double dropped = %d, want 2", res.DoubleDropped)
	}
	for _, d := range res.Media {
		p, err := transport.DecodeMedia(d)
		if err != nil {
			t.Fatal(err)
		}
		rep.Add(p)
	}
	for _, d := range res.Parity {
		h, xor, err := transport.DecodeParity(d)
		if err != nil {
			t.Fatal(err)
		}
		if rep.FeedParity(h, xor) {
			t.Fatalf("group %d should not be recoverable with two losses", h.BaseSeq/group)
		}
	}
	rep.Finish()
	if rep.Unrecovered != 2 {
		t.Fatalf("unrecovered = %d, want 2", rep.Unrecovered)
	}
	if !faults.Has(media.CodeRepairOverflow) {
		t.Fatalf("faults = %v, want repair_overflow", faults)
	}
	if !faults.Has(media.CodePacketGap) {
		t.Fatalf("faults = %v, want packet_gap", faults)
	}
	if rep.RecoveredPayloadPct() != 0 {
		t.Fatalf("recovered pct = %.1f, want 0", rep.RecoveredPayloadPct())
	}
}

func TestRepairRejectsInvalidGroupSize(t *testing.T) {
	if _, err := transport.NewRepairer(1, 1, nil); err == nil {
		t.Fatal("group size 1 must be rejected")
	}
	if _, err := transport.NewRepairer(1, 257, nil); err == nil {
		t.Fatal("group size above 256 must be rejected")
	}
}

func TestConfigHardCaps(t *testing.T) {
	c := transport.DefaultConfig()
	c.MaxBufferBytes = transport.HardMaxBufferBytes + 1
	if err := c.Validate(); err == nil {
		t.Fatal("buffer cap above 8 MiB must be rejected")
	}
	c = transport.DefaultConfig()
	c.GroupSize = 1
	if err := c.Validate(); err == nil {
		t.Fatal("group size 1 must be rejected")
	}
}
