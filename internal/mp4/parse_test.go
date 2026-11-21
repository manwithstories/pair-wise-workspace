package mp4_test

import (
	"errors"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/mp4"
)

// validFragment pulls one generated valid fragment out of the stream.
func validFragments(t *testing.T) [][]byte {
	t.Helper()
	s := fixture.Generate(fixture.Options{Seed: 7, Fragments: 4, Bandwidth: []float64{3}})
	seen := map[uint16]bool{}
	var out [][]byte
	for _, a := range s.Arrivals {
		if seen[a.Packet.SequenceNumber] {
			continue
		}
		seen[a.Packet.SequenceNumber] = true
		out = append(out, a.Packet.Payload)
	}
	return out
}

func TestParseValidFragments(t *testing.T) {
	frags := validFragments(t)
	if len(frags) == 0 {
		t.Fatal("no fragments generated")
	}
	for i, data := range frags {
		info, err := mp4.Parse(data)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		if info.Seq == 0 {
			t.Fatalf("fragment %d: mfhd sequence not extracted", i)
		}
		if len(info.Samples) == 0 {
			t.Fatalf("fragment %d: trun entries not extracted", i)
		}
		if info.Duration() == 0 {
			t.Fatalf("fragment %d: zero duration", i)
		}
		if len(info.Payload) == 0 {
			t.Fatalf("fragment %d: mdat payload missing", i)
		}
	}
}

func TestParseCorruptCases(t *testing.T) {
	s := fixture.Generate(fixture.Options{Seed: 7, Fragments: 4, Bandwidth: []float64{3}, Case: "corrupt"})
	if s.CorruptFrags != 4 {
		t.Fatalf("corrupt frags=%d want 4", s.CorruptFrags)
	}
	// The last four unique arrivals are the corrupt fragments.
	var corrupt [][]byte
	for _, a := range s.Arrivals {
		if a.Packet.SequenceNumber >= 1000+8 {
			corrupt = append(corrupt, a.Packet.Payload)
		}
	}
	if len(corrupt) != 4 {
		t.Fatalf("got %d corrupt arrivals, want 4", len(corrupt))
	}
	want := []error{mp4.ErrBoxOverflow, mp4.ErrMissingMfhd, mp4.ErrDupMfhdBox, nil}
	for i, data := range corrupt {
		_, err := mp4.Parse(data)
		if want[i] == nil {
			// tfdt-overflow fragment parses fine; the timeline
			// stage is what rejects it.
			if err != nil {
				t.Fatalf("case %d: unexpected parse error %v", i, err)
			}
			continue
		}
		if !errors.Is(err, want[i]) {
			t.Fatalf("case %d: want %v, got %v", i, want[i], err)
		}
	}
}

func TestWalkBoxesRejectsTruncation(t *testing.T) {
	// A single box header declaring more than available.
	data := []byte{0, 0, 0, 100, 'f', 'r', 'e', 'e', 0}
	if _, err := mp4.Parse(data); !errors.Is(err, mp4.ErrBoxOverflow) {
		t.Fatalf("want ErrBoxOverflow, got %v", err)
	}
	// Box smaller than its header.
	data = []byte{0, 0, 0, 4, 'f', 'r', 'e', 'e'}
	if _, err := mp4.Parse(data); !errors.Is(err, mp4.ErrBoxTooSmall) {
		t.Fatalf("want ErrBoxTooSmall, got %v", err)
	}
	// Truncated header.
	data = []byte{0, 0, 0}
	if _, err := mp4.Parse(data); !errors.Is(err, mp4.ErrBoxOverflow) {
		t.Fatalf("want ErrBoxOverflow, got %v", err)
	}
}

func TestParseMissingMoof(t *testing.T) {
	// styp only: no moof at all.
	data := []byte{0, 0, 0, 8, 's', 't', 'y', 'p'}
	if _, err := mp4.Parse(data); !errors.Is(err, mp4.ErrMissingMfhd) {
		t.Fatalf("want ErrMissingMfhd, got %v", err)
	}
}
