package segment

import (
	"bytes"
	"testing"

	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/sample"
	"github.com/archive/tsrestore/internal/timeline"
)

// collect runs the ordered RTP stream for a rendition through a fresh Builder.
func collect(t *testing.T, stream []byte, rendition string) ([]*Segment, *Builder) {
	t.Helper()
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	reader.OnSR = func(sr rtp.SenderReport) {
		_ = sr // anchors are fed to the clock by the pipeline in production.
	}
	clock := timeline.NewClock(timeline.RTPClockRate)
	b := NewBuilder(rendition, clock)
	var segs []*Segment
	for {
		p, err := reader.Next()
		if err != nil {
			break
		}
		made, perr := b.Push(p)
		if perr != nil {
			t.Fatalf("push: %v", perr)
		}
		segs = append(segs, made...)
	}
	if tail := b.Flush(); tail != nil {
		segs = append(segs, tail)
	}
	return segs, b
}

func TestCleanStreamCutsSixSecondSegments(t *testing.T) {
	r := sample.DefaultRenditions()[0]
	const seconds = 60
	stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(seconds), 41, sample.DamageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	segs, b := collect(t, stream, r.Name)
	// 60 s / 6 s = 10 segments. The last one is emitted by Flush at program
	// end (no following IDR to cut on), so only the first nine are guaranteed
	// key-aligned; the flush tail still hashes and stays TS aligned.
	if len(segs) != seconds/targetSecs {
		t.Fatalf("segments = %d, want %d", len(segs), seconds/targetSecs)
	}
	for i, s := range segs {
		if i < len(segs)-1 && !s.KeyAligned {
			t.Fatalf("non-tail segment %d not key aligned", i)
		}
		if d := s.DurationSecs - targetSecs; d > 0.045 || d < -0.045 {
			t.Fatalf("segment %d duration %.4f off target", i, s.DurationSecs)
		}
		if len(s.SHA256) != 64 {
			t.Fatalf("segment %d bad hash len", i)
		}
		if len(s.Data)%tsSize != 0 {
			t.Fatalf("segment %d data not TS aligned", i)
		}
	}
	if b.Stats().SyncLoss != 0 {
		t.Fatalf("clean stream had sync loss: %+v", b.Stats())
	}
}

func TestDeterministicHashesAcrossRuns(t *testing.T) {
	r := sample.DefaultRenditions()[0]
	mk := func() []*Segment {
		stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(30), 99, sample.DamageConfig{})
		if err != nil {
			t.Fatal(err)
		}
		segs, _ := collect(t, stream, r.Name)
		return segs
	}
	a, b := mk(), mk()
	if len(a) != len(b) {
		t.Fatalf("segment count drift %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].SHA256 != b[i].SHA256 {
			t.Fatalf("segment %d hash differs across runs", i)
		}
	}
}

func TestLossyStreamStillProducesSegmentsAndResyncs(t *testing.T) {
	r := sample.DefaultRenditions()[1]
	stream, st, err := sample.BuildRendition(r, sample.ProgramDuration(60), 41,
		sample.DamageConfig{Loss: 0.03, ReorderDist: 5, Duplicates: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	if st.Lost == 0 {
		t.Fatal("fixture should contain loss")
	}
	segs, b := collect(t, stream, r.Name)
	if len(segs) < secondsTargetCount(60) {
		t.Fatalf("recovered segments %d, want at least %d", len(segs), secondsTargetCount(60))
	}
	// Every emitted segment must remain key aligned because loss spares IDRs.
	for i, s := range segs {
		if !s.KeyAligned && i < len(segs)-1 {
			t.Fatalf("non-tail segment %d lost key alignment", i)
		}
	}
	if b.Stats().IDRFrames == 0 {
		t.Fatal("no IDR frames detected")
	}
}

func TestResyncRecoversFromGarbagePrefix(t *testing.T) {
	// Build one valid TS packet and prefix garbage to force a sync loss.
	r := sample.DefaultRenditions()[0]
	stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(12), 5, sample.DamageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the first TS payload's leading bytes inside the first RTP packet.
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	first, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	first.Payload[0] = 0x00
	first.Payload[1] = 0x11
	clock := timeline.NewClock(timeline.RTPClockRate)
	b := NewBuilder(r.Name, clock)
	made, err := b.Push(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 0 {
		t.Fatal("no segment should be emitted from the first packet")
	}
	if b.Stats().SyncLoss == 0 || b.Stats().Resynced == 0 {
		t.Fatalf("expected sync loss then resync, got %+v", b.Stats())
	}
}

func secondsTargetCount(seconds int) int { return seconds / targetSecs }
