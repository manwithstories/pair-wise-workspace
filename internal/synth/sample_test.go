package synth

import (
	"testing"

	"cinepipe/internal/ts"
)

func TestNormalSampleStructure(t *testing.T) {
	s := Build(SampleNormal, Defaults(SampleNormal, 42))
	if len(s.Data)%ts.PacketSize != 0 {
		t.Fatalf("data length %d not a multiple of %d", len(s.Data), ts.PacketSize)
	}
	// Every packet must start with the sync byte.
	for off := 0; off+ts.PacketSize <= len(s.Data); off += ts.PacketSize {
		if s.Data[off] != ts.SyncByte {
			t.Fatalf("packet at %d has sync 0x%02x", off, s.Data[off])
		}
	}
}

func (s *Sample) FrameCount() int { return len(s.FrameTicks) }

func TestNormalSampleKeyframeCadence(t *testing.T) {
	s := Build(SampleNormal, Defaults(SampleNormal, 1))
	// 60 frames with GOP 12: keyframes at 0,12,24,36,48.
	want := 5
	if len(s.KeyframeTicks) != want {
		t.Fatalf("keyframes %d want %d", len(s.KeyframeTicks), want)
	}
	// Intervals should be uniform.
	for i := 1; i < len(s.KeyframeTicks); i++ {
		if got := s.KeyframeTicks[i] - s.KeyframeTicks[i-1]; got != s.KeyframeInterval {
			t.Fatalf("interval %d want %d", got, s.KeyframeInterval)
		}
	}
}

func TestDeterminismBySeed(t *testing.T) {
	a := Build(SampleNormal, Defaults(SampleNormal, 42))
	b := Build(SampleNormal, Defaults(SampleNormal, 42))
	if string(a.Data) != string(b.Data) {
		t.Fatal("same seed produced different bytes")
	}
	c := Build(SampleNormal, Defaults(SampleNormal, 43))
	if string(a.Data) == string(c.Data) {
		t.Fatal("different seed produced identical bytes")
	}
}

func TestContinuitySampleHasDrops(t *testing.T) {
	s := Build(SampleContinuity, Defaults(SampleContinuity, 42))
	if s.IntendedDrops == 0 {
		t.Fatal("continuity sample should drop packets")
	}
}

func TestTruncatedSampleTail(t *testing.T) {
	s := Build(SampleTruncated, Defaults(SampleTruncated, 42))
	if len(s.Data)%ts.PacketSize == 0 {
		t.Fatal("truncated sample should end mid-packet")
	}
	if s.IntendedTruncation != 60 {
		t.Fatalf("truncation %d", s.IntendedTruncation)
	}
}

func TestWrapSamplePCRNearModulus(t *testing.T) {
	s := Build(SampleWrap, Defaults(SampleWrap, 42))
	if s.PCRBase < uint64(1<<33)-500000 {
		t.Fatalf("pcr base %d not near the modulus", s.PCRBase)
	}
}
