package parse

import (
	"bytes"
	"testing"
)

// box 手工构造 ISO BMFF 盒，便于精确制造损坏。
func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = byte(len(out) >> 24)
	out[1] = byte(len(out) >> 16)
	out[2] = byte(len(out) >> 8)
	out[3] = byte(len(out))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func nested(typ string, children ...[]byte) []byte {
	var p []byte
	for _, c := range children {
		p = append(p, c...)
	}
	return box(typ, p)
}

func TestWalkValidNested(t *testing.T) {
	data := nested("moov",
		box("mvhd", make([]byte, 10)),
		nested("trak", box("tkhd", make([]byte, 4))),
	)
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	if rep.HasFatal() {
		t.Fatalf("unexpected fatal: %v", rep.Errors)
	}
	if len(roots) != 1 || roots[0].Type() != "moov" || len(roots[0].Children) != 2 {
		t.Fatalf("tree wrong: %+v", roots)
	}
	if got := roots[0].Find("trak", "tkhd"); got == nil {
		t.Fatal("nested path lookup failed")
	}
	if rep.BoxesParsed != 4 {
		t.Fatalf("parsed=%d want 4", rep.BoxesParsed)
	}
}

func TestWalkTruncatedTail(t *testing.T) {
	good := box("ftyp", make([]byte, 8))
	// 声明 200 字节但实际只有 6 字节净荷 -> 尾部截断。
	bad := box("mdat", make([]byte, 6))
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 200
	data := append(append([]byte{}, good...), bad...)
	r := bytes.NewReader(data)
	roots, rep := Walk(r, DefaultLimits())
	if !rep.Truncated {
		t.Fatal("truncation must be flagged")
	}
	if len(roots) != 1 || roots[0].Type() != "ftyp" {
		t.Fatalf("last complete box must be retained: %+v", roots)
	}
	if rep.HasFatal() {
		t.Fatal("tail truncation is recoverable, not fatal")
	}
	rc := Recover(r, roots, int64(len(data)), rep)
	if rc.LastGoodBox != "ftyp" || rc.Reclaimed != int64(len(good)) {
		t.Fatalf("recovery wrong: %+v", rc)
	}
	if rc.LostTail <= 0 {
		t.Fatal("lost tail must be quantified")
	}
}

func TestWalkBadBoxDoesNotDiscardEarlier(t *testing.T) {
	a := box("free", []byte{1, 2, 3})
	bad := box("moov", []byte{0xff}) // 声明大小与净荷内部无关，制造越界
	bad[0], bad[1], bad[2], bad[3] = 0, 0, 0, 250
	data := append(append([]byte{}, a...), bad...)
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	if len(roots) != 1 {
		t.Fatalf("good prefix box lost: %+v", roots)
	}
	if len(rep.Boundaries) == 0 || rep.Boundaries[0].Kind != "bad_box" {
		t.Fatalf("bad box boundary missing: %+v", rep.Boundaries)
	}
}

func TestWalkHalfHeaderAtTail(t *testing.T) {
	data := append(box("ftyp", nil), []byte{0, 0, 0, 16, 'm', 'd'}...)
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	if !rep.Truncated || len(roots) != 1 {
		t.Fatalf("half header must end walk gracefully: %+v %+v", roots, rep.Boundaries)
	}
}

func TestWalkDepthAndBoxLimits(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxDepth = 2
	// 真实容器链：moov(0)->trak(1)->mdia(2)->minf(3)，mdia 处触顶。
	deep := nested("moov", nested("trak", nested("mdia", nested("minf", box("vmhd", nil)))))
	roots, rep := Walk(bytes.NewReader(deep), lim)
	if rep.SkippedBoxes == 0 {
		t.Fatal("depth limit must produce skipped record")
	}
	if roots[0].Children[0].Children == nil {
		t.Fatal("depth-2 node should still exist")
	}

	lim2 := DefaultLimits()
	lim2.MaxBoxes = 2
	_, rep2 := Walk(bytes.NewReader(nested("moov", box("mvhd", nil), box("tkhd", nil))), lim2)
	if rep2.SkippedBoxes == 0 {
		t.Fatal("box count limit must be enforced")
	}
}

func TestOversizedFileRejected(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxFileSize = 4
	_, rep := Walk(bytes.NewReader(box("ftyp", nil)), lim)
	if !rep.HasFatal() {
		t.Fatal("oversized file must be fatal")
	}
}

func TestSafePrefixAndErrors(t *testing.T) {
	rep := &Report{Boundaries: []Boundary{
		{Offset: 100, Kind: "bad_box"}, {Offset: 60, Kind: "truncated"},
	}}
	if got := SafePrefix(rep, 500); got != 60 {
		t.Fatalf("safe prefix = %d want 60", got)
	}
	rep.add(&Error{Level: LevelSkipped, BoxType: "foo"})
	rep.add(&Error{Level: LevelRecoverable, BoxType: "stts"})
	if rep.TimelineErrors() != 1 || rep.SkippedBoxes != 1 {
		t.Fatalf("counters wrong: %+v", rep)
	}
}
