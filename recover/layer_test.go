package recover

import (
	"bytes"
	"testing"

	"medcheck/fault"
)

func originalBlocks(n int) [][]byte {
	blocks := make([][]byte, n)
	for i := range blocks {
		blocks[i] = bytes.Repeat([]byte{byte('A' + i)}, 16)
	}
	return blocks
}

func TestInterleavedParityRecoversFourPacketBurst(t *testing.T) {
	orig := originalBlocks(8)
	seg := NewSegmentRepair(7, 4, orig)
	lost := map[int]bool{2: true, 3: true, 4: true, 5: true}
	for i, b := range orig {
		if !lost[i] {
			seg.Receive(i, b, true)
		}
	}
	got, bad := seg.Recover()
	if len(got) != 4 || len(bad) != 0 {
		t.Fatalf("recovered=%v bad=%v, want four recoveries", got, bad)
	}
	for i := range orig {
		if !bytes.Equal(seg.Data(i), orig[i]) {
			t.Fatalf("block %d reconstructed incorrectly", i)
		}
	}
	if !seg.Complete() || len(seg.SHA256()) != 64 {
		t.Fatalf("segment should be complete with sha256, complete=%v", seg.Complete())
	}
}

func TestTwoLossesInOneParityRowMarkPreciseBadSegment(t *testing.T) {
	fm := fault.NewModel()
	layer := NewLayer("1x", fm)
	orig := originalBlocks(8)
	seg := NewSegmentRepair(3, 4, orig)
	// Burst 2..5 plus CRC failure at 7: positions 3 and 7 share row 3.
	missing := map[int]bool{2: true, 3: true, 4: true, 5: true, 7: true}
	for i, b := range orig {
		if missing[i] {
			if i == 7 {
				seg.Receive(i, b, false)
			}
			continue
		}
		seg.Receive(i, b, true)
	}
	layer.AddSegment(seg)
	layer.RecoverAll()
	if len(layer.BadSegments) != 1 || layer.BadSegments[0] != 3 {
		t.Fatalf("bad segments=%v want [3]", layer.BadSegments)
	}
	if fm.Count(fault.BadSegment) != 1 {
		t.Fatalf("fault model bad segments=%d", fm.Count(fault.BadSegment))
	}
}

func TestLayersRecoverIndependently(t *testing.T) {
	fm := fault.NewModel()
	for _, name := range []string{"1x", "2x", "4x"} {
		l := NewLayer(name, fm)
		orig := originalBlocks(8)
		s := NewSegmentRepair(0, 4, orig)
		for i, b := range orig {
			if i < 4 { // burst positions 0..3, one per interleaved row
				continue
			}
			s.Receive(i, b, true)
		}
		l.AddSegment(s)
		l.RecoverAll()
		if l.RecoveredBlocks != 4 || len(l.BadSegments) != 0 {
			t.Fatalf("layer %s recovered=%d bad=%v", name, l.RecoveredBlocks, l.BadSegments)
		}
	}
}
