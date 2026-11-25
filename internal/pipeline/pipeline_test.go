package pipeline

import (
	"bytes"
	"context"
	"testing"

	"github.com/pairwise/mediapack/internal/audio"
	"github.com/pairwise/mediapack/internal/fixture"
)

func TestGeneratedFaultSampleCrossModule(t *testing.T) {
	s, err := fixture.Generate(fixture.Config{
		Seed:   73,
		Faults: []string{"pcr-wrap", "cc-gap", "psi-update", "bad-crc", "truncated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), bytes.NewReader(s.TS), Options{
		BufferPackets: 4096,
		Silence:       audio.Config{SampleRate: fixture.SampleRate, MinSilenceMS: fixture.SilenceMS},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Truncated == nil {
		t.Fatal("expected truncated tail report")
	}
	if rep.BadCRC != 1 {
		t.Fatalf("bad CRC=%d, want 1", rep.BadCRC)
	}
	if rep.PCRWraps != 1 {
		t.Fatalf("PCR wraps=%d, want 1", rep.PCRWraps)
	}
	if len(rep.Gaps) != 1 {
		t.Fatalf("CC gaps=%+v, want one", rep.Gaps)
	}
	if rep.PSIVersion != 2 {
		t.Fatalf("PSI version=%d, want 2", rep.PSIVersion)
	}
	if rep.Cut == nil || len(rep.Cut.Segments) != 4 {
		t.Fatalf("segments=%+v rejected=%v", rep.Cut, rep.Cut.Rejected)
	}
	for i, seg := range rep.Cut.Segments {
		if !seg.Committable || len(seg.ClipSHA256) != 64 {
			t.Fatalf("segment %d bad: %+v", i, seg)
		}
		if len(seg.Keyframes) != 1 || seg.Keyframes[0].PSIVersion == 0 {
			t.Fatalf("bad keyframe index: %+v", seg.Keyframes)
		}
	}
	if len(rep.Cut.Segments[0].Markers) == 0 {
		t.Fatal("CC gap marker did not reach cut/index metadata")
	}
	if len(rep.Cut.Segments[1].Silence) == 0 {
		t.Fatal("expected silence interval in PSI-v2 audio clip")
	}
	for i := 1; i < 4; i++ {
		if rep.Cut.Segments[i].AudioFrames != 1 || len(rep.Cut.Segments[i].Silence) != 1 {
			t.Fatalf("segment %d audio frames=%d silence=%+v pcm=%d",
				i, rep.Cut.Segments[i].AudioFrames, rep.Cut.Segments[i].Silence, len(rep.Cut.Segments[i].AudioPCM))
		}
	}
	if rep.Cut.Segments[0].ClipSHA256 != rep.Cut.Segments[3].ClipSHA256 {
		t.Fatalf("duplicate bodies hashes differ: %s vs %s",
			rep.Cut.Segments[0].ClipSHA256, rep.Cut.Segments[3].ClipSHA256)
	}
}
