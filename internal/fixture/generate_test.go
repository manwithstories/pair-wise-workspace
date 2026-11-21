package fixture

import (
	"testing"

	"mediacheck/internal/mp4"
)

func TestGenerateDeterministic(t *testing.T) {
	opt := Options{Seed: 41, Fragments: 6, LossBurst: 3, Bandwidth: []float64{3, 1, 6}}
	a := Generate(opt)
	b := Generate(opt)
	if len(a.Arrivals) != len(b.Arrivals) {
		t.Fatalf("arrival count differs: %d vs %d", len(a.Arrivals), len(b.Arrivals))
	}
	for i := range a.Arrivals {
		if a.Arrivals[i].ArrivalMs != b.Arrivals[i].ArrivalMs ||
			a.Arrivals[i].Packet.SequenceNumber != b.Arrivals[i].Packet.SequenceNumber {
			t.Fatalf("arrival %d differs between runs", i)
		}
	}
}

func TestGeneratedFragmentsParse(t *testing.T) {
	s := Generate(Options{Seed: 1, Fragments: 4, Bandwidth: []float64{3}})
	seen := map[uint16]bool{}
	n := 0
	for _, a := range s.Arrivals {
		seq := a.Packet.SequenceNumber
		if seen[seq] {
			continue
		}
		seen[seq] = true
		if _, err := mp4.Parse(a.Packet.Payload); err != nil {
			t.Fatalf("valid fixture failed to parse: seq=%d: %v", seq, err)
		}
		n++
	}
	if n != 8 { // 4 fragments x 2 tracks
		t.Fatalf("unique fragments=%d want 8", n)
	}
}

func TestLossModelCounts(t *testing.T) {
	s := Generate(Options{Seed: 41, Fragments: 8, LossBurst: 7, Bandwidth: []float64{3, 1, 6}})
	if s.TotalLost < 7 {
		t.Fatalf("total lost=%d want at least the burst of 7", s.TotalLost)
	}
	// Duplicates are generated deterministically (every 4th packet).
	dups := 0
	seen := map[uint16]bool{}
	for _, a := range s.Arrivals {
		if seen[a.Packet.SequenceNumber] {
			dups++
		}
		seen[a.Packet.SequenceNumber] = true
	}
	if dups == 0 {
		t.Fatal("expected deterministic duplicate arrivals")
	}
}

func TestCorruptCaseCount(t *testing.T) {
	s := Generate(Options{Seed: 1, Fragments: 4, Case: "corrupt", Bandwidth: []float64{3}})
	if s.CorruptFrags != 4 {
		t.Fatalf("corrupt frags=%d want 4", s.CorruptFrags)
	}
}

func TestAudioPCMSilence(t *testing.T) {
	pcm, _ := audioPCM(100, 0, [2]int{-1, -1}, true)
	for i, b := range pcm {
		if b != 0 {
			t.Fatalf("full-silence payload has nonzero byte at %d", i)
		}
	}
	pcm, _ = audioPCM(100, 0, [2]int{40, 60}, false)
	for i := 40 * 4; i < 60*4; i++ {
		if pcm[i] != 0 {
			t.Fatalf("silent interval has nonzero byte at %d", i)
		}
	}
	// Sample 10 of a 440 Hz sine at phase 0 is far from a zero crossing.
	if pcm[40] == 0 && pcm[41] == 0 && pcm[42] == 0 && pcm[43] == 0 {
		t.Fatal("non-silent region unexpectedly zero")
	}
}

func TestVideoPayloadDeterministicDup(t *testing.T) {
	a := videoPayload(5, 128, -1)
	b := videoPayload(5, 128, -1)
	c := videoPayload(9, 128, 5) // duplicates fragment 5
	for i := range a {
		if a[i] != b[i] || a[i] != c[i] {
			t.Fatalf("payload mismatch at %d", i)
		}
	}
	d := videoPayload(6, 128, -1)
	same := true
	for i := range a {
		if a[i] != d[i] {
			same = false
		}
	}
	if same {
		t.Fatal("different fragments produced identical payloads")
	}
}
