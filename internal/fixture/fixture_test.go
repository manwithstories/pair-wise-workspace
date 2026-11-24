package fixture

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/pion/rtp"

	"review/internal/ebml"
)

// walkAll drives the streaming reader over a full document and counts
// block elements; any truncation/boundary error fails the test.
func walkAll(t *testing.T, raw []byte) (blocks int) {
	t.Helper()
	rd := ebml.NewReader(bytes.NewReader(raw))
	depth := 0
	for {
		h, err := rd.Next()
		if errors.Is(err, io.EOF) {
			if depth == 0 {
				return blocks
			}
			if err := rd.Leave(); err != nil {
				t.Fatalf("leave: %v", err)
			}
			depth--
			continue
		}
		if err != nil {
			t.Fatalf("walk error at offset %d: %v", rd.Offset(), err)
		}
		if ebml.IsMaster(h.ID) {
			if err := rd.Enter(h); err != nil {
				t.Fatalf("enter 0x%X: %v", h.ID, err)
			}
			depth++
			continue
		}
		if h.ID == ebml.IDSimpleBlock || h.ID == ebml.IDBlock {
			blocks++
		}
		if err := rd.Skip(h); err != nil {
			t.Fatalf("skip 0x%X: %v", h.ID, err)
		}
	}
}

func TestValidSamplesParseCleanly(t *testing.T) {
	samples := map[string]Document{
		SampleStudio:     StudioInterview(42),
		SampleLocation:   LocationBRoll(42),
		SampleRemote:     RemoteNarration(42),
		SampleColourClash: ColourClash(42),
		SampleAudioClash: AudioClash(42),
	}
	for name, doc := range samples {
		raw := doc.Build()
		if len(raw) == 0 {
			t.Fatalf("%s: empty document", name)
		}
		if n := walkAll(t, raw); n == 0 {
			t.Fatalf("%s: no blocks discovered", name)
		}
		// Determinism: same seed must yield byte-identical output.
		if again := doc.Build(); !bytes.Equal(raw, again) {
			t.Fatalf("%s: non-deterministic build", name)
		}
	}
}

func TestUnknownSizeFramesCloseAtEOF(t *testing.T) {
	doc := RemoteNarration(7)
	doc.UnknownSegment = true
	doc.UnknownCluster = true
	raw := doc.Build()
	if n := walkAll(t, raw); n == 0 {
		t.Fatal("unknown-size document yielded no blocks")
	}
}

func TestTruncationAndBoundaryCorruptions(t *testing.T) {
	t.Run("tail truncation marks first missing offset", func(t *testing.T) {
		raw := TruncateTail(LocationBRoll(42))
		err := expectWalkError(raw)
		var te *ebml.TruncatedError
		if !errors.As(err, &te) {
			t.Fatalf("want TruncatedError, got %v", err)
		}
		if te.MissingOffset != int64(len(raw)) {
			t.Fatalf("missing offset %d, want %d", te.MissingOffset, len(raw))
		}
	})
	t.Run("out-of-range block trips boundary", func(t *testing.T) {
		raw := OutOfRangeBlock(StudioInterview(42))
		err := expectWalkError(raw)
		var be *ebml.BoundaryError
		if !errors.As(err, &be) {
			t.Fatalf("want BoundaryError, got %v", err)
		}
	})
	t.Run("unknown-frame truncation is surfaced", func(t *testing.T) {
		raw := TruncateUnknown(42)
		if err := expectWalkError(raw); err == nil {
			t.Fatal("truncated unknown frame must not parse cleanly")
		}
	})
}

// expectWalkError walks a corrupt document and returns the first error.
func expectWalkError(raw []byte) error {
	rd := ebml.NewReader(bytes.NewReader(raw))
	depth := 0
	for {
		h, err := rd.Next()
		if errors.Is(err, io.EOF) {
			if depth == 0 {
				return nil
			}
			if err := rd.Leave(); err != nil {
				return err
			}
			depth--
			continue
		}
		if err != nil {
			return err
		}
		if ebml.IsMaster(h.ID) {
			if err := rd.Enter(h); err != nil {
				return err
			}
			depth++
		} else if err := rd.Skip(h); err != nil {
			return err
		}
	}
}

func TestRTPCaptureAndDuplicate(t *testing.T) {
	pkts := CaptureRTP(42, 24)
	if len(pkts) != 24 {
		t.Fatalf("packet count = %d", len(pkts))
	}
	var first rtp.Packet
	if err := first.Unmarshal(pkts[0]); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Strictly increasing sequence numbers in the clean capture.
	prev := first.SequenceNumber
	for _, raw := range pkts[1:] {
		var p rtp.Packet
		if err := p.Unmarshal(raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.SequenceNumber != prev+1 {
			t.Fatalf("seq gap: %d -> %d", prev, p.SequenceNumber)
		}
		prev = p.SequenceNumber
	}
	dup := RTPDuplicate(42)
	seen := map[uint16]int{}
	for _, raw := range dup {
		var p rtp.Packet
		if err := p.Unmarshal(raw); err != nil {
			t.Fatalf("unmarshal duplicate set: %v", err)
		}
		seen[p.SequenceNumber]++
	}
	duplicated := false
	for _, n := range seen {
		if n > 1 {
			duplicated = true
		}
	}
	if !duplicated {
		t.Fatal("RTPDuplicate must contain a repeated sequence number")
	}
}
