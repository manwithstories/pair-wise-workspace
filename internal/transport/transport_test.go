package transport

import (
	"testing"

	"mediaflow/internal/fixture"
	"mediaflow/internal/timeline"
)

// runReceiver feeds all damaged (and optionally duplicated) packets through a
// receiver and returns the final ordered output, gaps and stats.
func runReceiver(t *testing.T, clip *fixture.Clip, cfg Config) ([]fixture.Packet, []Gap, Stats) {
	t.Helper()
	r, err := NewReceiver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := fixture.ExpandDuplicates(clip.Packets)
	for _, p := range stream {
		if err := r.Ingest(p); err != nil {
			t.Fatal(err)
		}
	}
	out, gaps, st := r.Flush()
	return out, gaps, st
}

func TestCleanReorder(t *testing.T) {
	cfg := fixture.DefaultConfig(7)
	clip, err := fixture.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clip.Spec.ReorderWindow = 8
	damaged := fixture.ApplyDamage(clip)

	out, gaps, st := runReceiver(t, damaged, Config{ReorderWindow: 16})
	if len(gaps) != 0 || st.Gaps != 0 {
		t.Fatalf("clean stream produced gaps: %+v stats=%+v", gaps, st)
	}
	if st.Recovered != 0 || st.LostMedia != 0 {
		t.Fatalf("clean stream reported loss: %+v", st)
	}
	// Output must be globally time-ordered.
	for i := 1; i < len(out); i++ {
		if out[i].TrueUS < out[i-1].TrueUS {
			t.Fatalf("output not time-ordered at %d", i)
		}
	}
	// Every media packet delivered exactly once.
	wantMedia := 0
	for _, p := range clip.Packets {
		if !p.IsParity {
			wantMedia++
		}
	}
	if st.Delivered != wantMedia {
		t.Fatalf("delivered %d, want %d", st.Delivered, wantMedia)
	}
	if st.Reordered == 0 {
		t.Fatal("shuffled input produced no reorder count")
	}
}

func TestDuplicateSuppression(t *testing.T) {
	cfg := fixture.DefaultConfig(7)
	clip, _ := fixture.Generate(cfg)
	clip.Spec.DuplicateSeq = map[uint32][]uint32{1: {0, 1, 2}, 2: {10}}
	damaged := fixture.ApplyDamage(clip)
	_, _, st := runReceiver(t, damaged, Config{ReorderWindow: 16})
	if st.Duplicates != 4 {
		t.Fatalf("expected 4 duplicates collapsed, got %d", st.Duplicates)
	}
}

func TestFECSingleLossRecovery(t *testing.T) {
	cfg := fixture.DefaultConfig(7)
	clip, _ := fixture.Generate(cfg)
	// One media packet dropped in each of several distinct groups.
	clip.Spec.DropSingles = []int{0, 2, 4, 6}
	damaged := fixture.ApplyDamage(clip)
	out, gaps, st := runReceiver(t, damaged, Config{ReorderWindow: 16})

	if len(gaps) != 0 || st.Gaps != 0 {
		t.Fatalf("single losses must recover, got gaps=%+v", gaps)
	}
	if st.Recovered != 4 || st.LostMedia != 4 {
		t.Fatalf("recovery stats wrong: %+v", st)
	}
	// Every recovered packet must carry reconstructed payload, never zeroed.
	recovered := 0
	for _, p := range out {
		if p.Recovered {
			recovered++
			empty := true
			for _, b := range p.Payload {
				if b != 0 {
					empty = false
					break
				}
			}
			if empty {
				t.Fatal("recovered payload is empty: media data was fabricated")
			}
		}
	}
	if recovered != 4 {
		t.Fatalf("recovered packets in output: %d", recovered)
	}
}

func TestFECMultipleLossGap(t *testing.T) {
	cfg := fixture.DefaultConfig(7)
	clip, _ := fixture.Generate(cfg)
	// Two media packets in group 3 cannot be rebuilt from one parity packet.
	clip.Spec.DropMultiples = map[int]int{3: 2, 5: 3}
	damaged := fixture.ApplyDamage(clip)
	out, gaps, st := runReceiver(t, damaged, Config{ReorderWindow: 16})

	if st.Gaps != 2 {
		t.Fatalf("expected 2 gap records, got %d", st.Gaps)
	}
	totalMissing := 0
	for _, g := range gaps {
		totalMissing += g.Count
		if g.SeqTo < g.SeqFrom || g.Count != int(g.SeqTo-g.SeqFrom)+1 {
			t.Fatalf("gap identity imprecise: %+v", g)
		}
		if g.EndUS < g.StartUS && g.EndUS != 0 {
			t.Fatalf("gap time bounds inverted: %+v", g)
		}
	}
	if totalMissing != 5 {
		t.Fatalf("expected 5 missing slots, got %d", totalMissing)
	}
	// No recovered packet should claim the multi-loss slots.
	if st.Recovered != 0 {
		t.Fatalf("multi-loss must not fabricate recovery: %+v", st)
	}
	// All surviving media still delivered. Count from the damaged delivery
	// stream: the source clip never carries the Dropped markers.
	survivors := 0
	for _, p := range damaged.Packets {
		if !p.IsParity && !p.Dropped {
			survivors++
		}
	}
	if st.Delivered != survivors {
		t.Fatalf("delivered %d, survivors %d", st.Delivered, survivors)
	}
	if len(out) != survivors {
		t.Fatalf("output count %d != survivors %d", len(out), survivors)
	}
}

func TestWraparoundUnwrap(t *testing.T) {
	// Small modulus forces several rollovers on the sequence space.
	const modulus = 64
	r, err := NewReceiver(Config{ReorderWindow: 8, WrapModulus: modulus})
	if err != nil {
		t.Fatal(err)
	}
	var ingested []fixture.Packet
	for seq := int64(0); seq < 200; seq++ {
		raw := uint32(seq % modulus)
		p := fixture.Packet{
			Track: 9, Seq: raw, TrueUS: seq * 1000,
			FECGroup: int(seq / fixture.FECGroupSize), Payload: []byte{byte(seq)},
		}
		ingested = append(ingested, p)
	}
	for _, p := range ingested {
		if err := r.Ingest(p); err != nil {
			t.Fatal(err)
		}
	}
	out, gaps, st := r.Flush()
	if len(gaps) != 0 {
		t.Fatalf("wrap stream must not gap: %+v", gaps)
	}
	if st.Wraps < 2 {
		t.Fatalf("expected multiple wraps, got %d", st.Wraps)
	}
	if len(out) != 200 {
		t.Fatalf("expected 200 delivered across wraps, got %d", len(out))
	}
}

func TestReorderWindowLateArrival(t *testing.T) {
	r, _ := NewReceiver(Config{ReorderWindow: 4, WrapModulus: 0})
	// Sequence 1 is genuinely missing; the high watermark advances well past
	// the reorder window so expiry declares it lost before Flush.
	for _, seq := range []int64{0, 2, 3, 4, 5, 6, 7} {
		_ = r.Ingest(fixture.Packet{Track: 1, Seq: uint32(seq), TrueUS: seq * 1000,
			FECGroup: int(seq / fixture.FECGroupSize), Payload: []byte{byte(seq)}})
	}
	if st := r.Stats(); st.LostMedia == 0 {
		t.Fatalf("window expiry should mark the missing sequence: %+v", st)
	}
	// A late duplicate of the flushed sequence must be ignored.
	_ = r.Ingest(fixture.Packet{Track: 1, Seq: 1, TrueUS: 1000,
		FECGroup: 0, Payload: []byte{99}})
	out, gaps, _ := r.Flush()
	if len(gaps) == 0 {
		t.Fatalf("missing sequence should surface as a gap: out=%d", len(out))
	}
}

func TestInvalidConfig(t *testing.T) {
	_, err := NewReceiver(Config{ReorderWindow: 0})
	if err == nil {
		t.Fatal("expected error for zero reorder window")
	}
	if te, ok := timeline.AsError(err); !ok || te.Track != 0 {
		t.Fatalf("expected structured global error, got %v", err)
	}
}

func TestParityOnlyXORRecoveryMatchesOriginal(t *testing.T) {
	cfg := fixture.DefaultConfig(7)
	clip, _ := fixture.Generate(cfg)
	group := 1
	clip.Spec.DropSingles = []int{group}
	damaged := fixture.ApplyDamage(clip)
	out, gaps, _ := runReceiver(t, damaged, Config{ReorderWindow: 16})
	if len(gaps) != 0 {
		t.Fatal(gaps)
	}
	var got fixture.Packet
	found := false
	for _, p := range out {
		if p.Recovered && p.Track == 1 && p.FECGroup == group {
			got, found = p, true
		}
	}
	if !found {
		t.Fatal("recovered packet not found")
	}
	// Identify the source packet actually removed: present in clean source,
	// absent from the damaged delivery stream.
	deliveredSeq := map[uint32]bool{}
	for _, p := range damaged.Packets {
		if p.Track == 1 && !p.IsParity {
			deliveredSeq[p.Seq] = true
		}
	}
	var expect []byte
	for _, p := range clip.Packets {
		if p.Track == 1 && !p.IsParity && p.FECGroup == group && !deliveredSeq[p.Seq] {
			expect = append(expect, p.Payload...)
		}
	}
	if string(got.Payload) != string(expect) {
		t.Fatalf("recovered payload mismatch\n got=%v\nwant=%v", got.Payload, expect)
	}
}
