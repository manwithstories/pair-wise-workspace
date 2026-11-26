package align_test

import (
	"testing"

	"avpipe/internal/align"
	"avpipe/internal/index"
	"avpipe/internal/media"
)

func testIndex(name string, bitrate int64, n int, dropSeq int, ccSeq int) *index.Index {
	idx := &index.Index{Rendition: name, Bitrate: bitrate, ExpectedSlots: n}
	for s := 0; s < n; s++ {
		if s == dropSeq {
			continue
		}
		idx.Keyframes = append(idx.Keyframes, index.Keyframe{
			Rendition: name, Seq: s, Tick: uint64(90000 + s*90000), Epoch: 0,
		})
	}
	if ccSeq >= 0 {
		idx.AffectedIntervals = append(idx.AffectedIntervals, index.AffectedInterval{
			Seq: ccSeq, Reason: media.CodeCCGap, Detail: "test gap",
		})
	}
	idx.MarkMissingSlots()
	return idx
}

func TestAlignCleanBoundaries(t *testing.T) {
	a := testIndex("r0", 600000, 12, -1, -1)
	b := testIndex("r1", 1200000, 12, -1, -1)
	l, err := align.Align([]*index.Index{a, b}, 12)
	if err != nil {
		t.Fatal(err)
	}
	if l.AlignedSlots != 12 || l.GapSlots != 0 {
		t.Fatalf("aligned=%d gap=%d", l.AlignedSlots, l.GapSlots)
	}
	for i, s := range l.Slots {
		if s.Seq != i || !s.Aligned || len(s.Boundaries) != 2 {
			t.Fatalf("slot %d bad: %+v", i, s)
		}
		if s.CommonTick != uint64(90000+i*90000) {
			t.Fatalf("slot %d tick=%d", i, s.CommonTick)
		}
	}
}

func TestAlignOneSidedMissingKeepsSequenceAndWritesGap(t *testing.T) {
	a := testIndex("r0", 600000, 12, 7, -1)
	b := testIndex("r1", 1200000, 12, -1, -1)
	l, err := align.Align([]*index.Index{a, b}, 12)
	if err != nil {
		t.Fatal(err)
	}
	if l.AlignedSlots != 11 || l.GapSlots != 1 {
		t.Fatalf("aligned=%d gap=%d want 11/1", l.AlignedSlots, l.GapSlots)
	}
	slot := l.Slots[7]
	if !slot.Gap || slot.Aligned {
		t.Fatalf("seq 7 not explicit gap: %+v", slot)
	}
	var present, missing int
	for _, bnd := range slot.Boundaries {
		if bnd.Present {
			present++
		} else {
			missing++
		}
	}
	if present != 1 || missing != 1 {
		t.Fatalf("boundaries present=%d missing=%d", present, missing)
	}
	if l.Slots[8].Seq != 8 || !l.Slots[8].Aligned {
		t.Fatal("sequence shifted after missing boundary")
	}
}

func TestAlignCCImpactForcesErrorPath(t *testing.T) {
	a := testIndex("r0", 600000, 12, -1, 3)
	b := testIndex("r1", 1200000, 12, -1, -1)
	l, err := align.Align([]*index.Index{a, b}, 12)
	if err != nil {
		t.Fatal(err)
	}
	if l.GapSlots != 1 || l.Slots[3].Aligned {
		t.Fatalf("CC interval did not enter gap path: gap=%d slot=%+v", l.GapSlots, l.Slots[3])
	}
}

func TestAlignRequiresTwoRenditions(t *testing.T) {
	_, err := align.Align(nil, 12)
	if err == nil || media.ExitCode(err) != 2 {
		t.Fatalf("err=%v exit=%d want config exit 2", err, media.ExitCode(err))
	}
}
