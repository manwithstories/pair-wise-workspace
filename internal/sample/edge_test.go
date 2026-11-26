package sample

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/timeline"
)

func TestEdgeTruncatedDatagramIsCorrupt(t *testing.T) {
	stream, err := BuildEdgeFixture(EdgeTruncated, 41, 12)
	if err != nil {
		t.Fatal(err)
	}
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	var sawCorrupt bool
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var te *timeline.Error
			if errors.As(err, &te) && te.Class == timeline.ClassCorrupt {
				sawCorrupt = true
				break
			}
			t.Fatalf("unexpected error class: %v", err)
		}
	}
	if !sawCorrupt {
		t.Fatal("truncated datagram must surface a Corrupt framing error")
	}
}

func TestEdgeBadRTCPIsCountedNotFatal(t *testing.T) {
	stream, err := BuildEdgeFixture(EdgeBadRTCP, 41, 12)
	if err != nil {
		t.Fatal(err)
	}
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	var rtpPackets int
	for {
		p, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("bad RTCP must not abort media reading: %v", err)
		}
		rtpPackets++
		_ = p
	}
	if reader.Stats().BadRTCP != 1 {
		t.Fatalf("BadRTCP = %d, want 1", reader.Stats().BadRTCP)
	}
	if rtpPackets == 0 {
		t.Fatal("media stream must still be readable after bad RTCP")
	}
}

func TestEdgePassthroughKinds(t *testing.T) {
	for _, kind := range []EdgeKind{EdgeTamperCache, EdgeSlowConsumer} {
		got, err := BuildEdgeFixture(kind, 41, 6)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		clean, _, err := BuildRendition(DefaultRenditions()[0], ProgramDuration(6), 41, DamageConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, clean) {
			t.Fatalf("%s should pass the clean fixture through unchanged", kind)
		}
	}
	if _, err := BuildEdgeFixture("nope", 1, 1); !errors.Is(err, ErrUnknownEdge) {
		t.Fatalf("unknown kind must error, got %v", err)
	}
}
