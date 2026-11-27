package nettrace

import (
	"math/rand"
	"testing"

	"avcheck/internal/wav"
)

func mkBlocks(n int) []*wav.Block {
	out := make([]*wav.Block, n)
	for i := range out {
		b := &wav.Block{Seq: uint64(i), Track: 0, Rate: 48000,
			Layout: wav.Stereo, Samples: []int16{int16(i), int16(-i)}}
		if i > 0 {
			b.Red[0] = wav.Redundancy{Seq: uint64(i - 1), Samples: out[i-1].Samples}
		}
		if i > 1 {
			b.Red[1] = wav.Redundancy{Seq: uint64(i - 2), Samples: out[i-2].Samples}
		}
		out[i] = b
	}
	return out
}

func arrived(blocks []*wav.Block, drop map[int]bool, seed int64) []*wav.Block {
	var out []*wav.Block
	for i, b := range blocks {
		if !drop[i] {
			out = append(out, b, b) // duplicate every arrival
		}
	}
	rand.New(rand.NewSource(seed)).Shuffle(len(out), func(i, j int) {
		out[i], out[j] = out[j], out[i]
	})
	return out
}

func TestRecoverIsolatedAndBurstTwo(t *testing.T) {
	blocks := mkBlocks(12)
	drop := map[int]bool{3: true, 7: true, 8: true} // isolated + burst of 2
	items, st := Recover(arrived(blocks, drop, 1), 0, 11)
	if st.Lost != 3 || st.Recovered != 3 || st.Gaps != 0 {
		t.Fatalf("stats %+v", st)
	}
	if st.Duplicates != 9 {
		t.Fatalf("duplicates %d", st.Duplicates)
	}
	for i, it := range items {
		if it.Gap || it.Block.Seq != uint64(i) {
			t.Fatalf("slot %d: %+v", i, it)
		}
		if len(it.Block.Samples) != 2 {
			t.Fatalf("slot %d missing samples", i)
		}
	}
}

func TestUnrecoverableBurstLeavesGap(t *testing.T) {
	blocks := mkBlocks(10)
	drop := map[int]bool{4: true, 5: true, 6: true} // burst of 3
	items, st := Recover(arrived(blocks, drop, 2), 0, 9)
	if st.Lost != 3 || st.Recovered != 2 || st.Gaps != 1 {
		t.Fatalf("stats %+v", st)
	}
	// seq 4 has no carrier (5,6 lost); 5 and 6 ride on block 7.
	if !items[4].Gap {
		t.Fatalf("seq 4 should be a gap: %+v", items[4])
	}
	if items[5].Gap || !items[5].Recovered || items[6].Gap || !items[6].Recovered {
		t.Fatalf("seq 5,6 should be recovered: %+v %+v", items[5], items[6])
	}
	// Order is preserved; the gap is a marker, not a shift.
	for i, it := range items {
		if it.Seq != uint64(i) {
			t.Fatalf("slot %d has seq %d", i, it.Seq)
		}
	}
}

func TestOutOfRangeIgnored(t *testing.T) {
	blocks := mkBlocks(4)
	extra := &wav.Block{Seq: 99, Rate: 48000, Layout: wav.Mono, Samples: []int16{1}}
	_, st := Recover(append(arrived(blocks, nil, 3), extra), 0, 3)
	if st.Received != 4 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
}
