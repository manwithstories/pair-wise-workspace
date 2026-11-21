package recovery

import (
	"testing"

	"avcheck/internal/clock"
)

const fp = 4 // frames per packet in these fixtures

func recvBlock(seq uint16, value int32) *clock.Block {
	s := make([]int32, fp)
	for i := range s {
		s[i] = value
	}
	return &clock.Block{TrackID: 1, SSRC: 7, Seq: seq, StartTick: int64(seq) * fp,
		Channels: 1, Samples: s, Source: clock.SourceOriginal}
}

func recvBlockCh(seq uint16, ch int, value int32) *clock.Block {
	s := make([]int32, fp*ch)
	for i := range s {
		s[i] = value
	}
	return &clock.Block{TrackID: 1, SSRC: 7, Seq: seq, StartTick: int64(seq) * fp,
		Channels: ch, Samples: s, Source: clock.SourceOriginal}
}

func TestThreePacketBurstIsInterpolated(t *testing.T) {
	// Stream with exactly three missing packets (seqs 1..3).
	r := NewReassembler(7, 1, 1, fp, 16)
	r.Offer(recvBlock(0, 0))
	r.Offer(recvBlock(4, 40))
	out := r.Emit()
	if len(out) != 5 { // anchor 0, 3 synth, anchor 4
		t.Fatalf("got %d blocks, want 5", len(out))
	}
	var interp int
	for _, b := range out {
		if b.Source == clock.SourceInterpolated {
			interp += b.Frames()
		}
	}
	// Gap of 12 frames must all be interpolated and contiguous with no overlap.
	if interp != 12 {
		t.Fatalf("interpolated frames=%d, want 12", interp)
	}
	if out[3].EndTick() != 4*fp {
		t.Fatalf("reconstructed end=%d, want %d", out[3].EndTick(), 4*fp)
	}
	// Global linear interpolation between anchor 0 (value 0) and anchor 4
	// (value 40): tick 8 sits halfway between the anchors' last/first samples
	// (span 16 ticks, pos 8) -> value 20.
	mid := out[2].Samples[0]
	if mid != 20 {
		t.Fatalf("midpoint sample=%d, want 20", mid)
	}
	st := r.Stats()
	if st.MissingPackets != 3 || st.Interpolated != 3 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestFourPacketBurstFallsBackToSilence(t *testing.T) {
	r := NewReassembler(7, 1, 1, fp, 32)
	r.Offer(recvBlock(0, 100))
	r.Offer(recvBlock(5, 500)) // seqs 1..4 missing -> 4 packets
	out := r.Emit()
	var silent int
	var total int
	for _, b := range out[1 : len(out)-1] { // skip both real anchors
		total += b.Frames()
		if b.Source != clock.SourceSilence {
			t.Fatalf("block seq=%d source=%d, want silence", b.Seq, b.Source)
		}
		for _, v := range b.Samples {
			if v != 0 {
				t.Fatalf("silence block contains %d", v)
			}
		}
		silent += b.Frames()
	}
	if silent != 16 || total != 16 {
		t.Fatalf("silent=%d total=%d, want 16/16", silent, total)
	}
	if r.Stats().SilenceFilled != 4 {
		t.Fatalf("silence packets=%d, want 4", r.Stats().SilenceFilled)
	}
}

func TestReconstructionPreservesTotalSampleCount(t *testing.T) {
	const ch = 2
	r := NewReassembler(7, 1, ch, fp, 32)
	// Build a 20-packet stream, drop seqs 2,3 and 9..12.
	dropped := map[uint16]bool{2: true, 3: true, 9: true, 10: true, 11: true, 12: true}
	for seq := uint16(0); seq < 20; seq++ {
		if dropped[seq] {
			continue
		}
		r.Offer(recvBlockCh(seq, ch, int32(seq)*10))
	}
	r.Flush(20)
	out := r.Emitted() // full reconstructed stream including streamed blocks
	var frames int
	var tick int64
	for _, b := range out {
		if b.StartTick != tick {
			t.Fatalf("gap/overlap at tick %d, block starts %d", tick, b.StartTick)
		}
		tick = b.EndTick()
		frames += b.Frames()
	}
	if frames != 20*fp {
		t.Fatalf("reconstructed frames=%d, want %d", frames, 20*fp)
	}
	st := r.Stats()
	if st.MissingPackets != 6 || st.Interpolated != 2 || st.SilenceFilled != 4 {
		t.Fatalf("stats=%+v, want missing=6 interp=2 silence=4", st)
	}
}
