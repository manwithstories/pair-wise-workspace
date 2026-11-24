package reorder_test

import (
	"math/rand"
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/reorder"
)

// TestGenerativeSequenceWrapLossDuplicates generates many random arrival
// patterns and checks order, gap accounting and monotonic media time.
func TestGenerativeSequenceWrapLossDuplicates(t *testing.T) {
	for seed := int64(1); seed <= 60; seed++ {
		t.Run("seed", func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			n := 120 + rng.Intn(900)
			samplesPerPacket := 480
			start := uint16(rng.Intn(1 << 16))
			if seed%2 == 0 {
				start = uint16(65500 + rng.Intn(36)) // force wrap-around
			}

			lost := map[int]bool{}
			for i := 0; i < n; i++ {
				if rng.Float64() < 0.08 {
					lost[i] = true
				}
			}
			// Keep both span endpoints present so every random loss is inside
			// the sequence range emitted by Flush.
			delete(lost, 0)
			delete(lost, n-1)
			order := make([]int, 0, n-len(lost))
			for i := 0; i < n; i++ {
				if !lost[i] {
					order = append(order, i)
				}
			}
			for s := 0; s < 1+rng.Intn(20); s++ {
				p := rng.Intn(len(order) - 1)
				order[p], order[p+1] = order[p+1], order[p]
			}

			clk, err := clock.New(48000)
			if err != nil {
				t.Fatal(err)
			}
			buf := reorder.NewBuffer(clk, 2, samplesPerPacket)
			push := func(i int) {
				seq := start + uint16(i)
				ts := uint32(12345) + uint32(i*samplesPerPacket)
				if err := buf.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts}}, int64(i)*20_000_000); err != nil {
					t.Fatal(err)
				}
			}
			for _, i := range order {
				push(i)
			}
			dupCount := 0
			for i := range order {
				if rng.Float64() < 0.08 {
					push(order[i])
					dupCount++
				}
			}

			blocks, stats, err := buf.Flush()
			if err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			if stats.Duplicate != dupCount {
				t.Fatalf("seed %d duplicates=%d want %d", seed, stats.Duplicate, dupCount)
			}
			if stats.Lost != len(lost) {
				t.Fatalf("seed %d lost=%d want %d", seed, stats.Lost, len(lost))
			}
			wantBlocks := n
			if len(blocks) != wantBlocks {
				t.Fatalf("seed %d blocks=%d want %d", seed, len(blocks), wantBlocks)
			}

			var lastSeq int64
			var lastTS int64
			missing := 0
			for i, b := range blocks {
				if i > 0 {
					if b.ExtSeq != lastSeq+1 {
						t.Fatalf("seed %d sequence gap at block %d", seed, i)
					}
					if b.ExtTS < lastTS {
						t.Fatalf("seed %d timestamp went backwards: %d < %d", seed, b.ExtTS, lastTS)
					}
				}
				lastSeq = b.ExtSeq
				lastTS = b.ExtTS
				if b.Missing {
					missing++
					if b.Samples != samplesPerPacket {
						t.Fatalf("seed %d bad gap duration %d", seed, b.Samples)
					}
				}
			}
			if missing != len(lost) {
				t.Fatalf("seed %d missing blocks=%d want %d", seed, missing, len(lost))
			}
		})
	}
}
