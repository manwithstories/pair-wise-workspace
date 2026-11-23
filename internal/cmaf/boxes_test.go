package cmaf

import (
	"encoding/binary"
	"testing"

	"mediaflow/internal/clock"
)

func mkBox(typ string, payload []byte) []byte {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head, uint32(8+len(payload)))
	copy(head[4:], typ)
	return append(head, payload...)
}

func TestParseBoxHeaderBounds(t *testing.T) {
	buf := mkBox("ftyp", make([]byte, 8))
	got, err := ParseBoxHeader(buf, 0, len(buf))
	if err != nil || !got.Valid || got.Size != 16 || got.Type != "ftyp" {
		t.Fatalf("header: %+v err %v", got, err)
	}
	// Declared size beyond limit stays parseable but invalid.
	binary.BigEndian.PutUint32(buf, 4096)
	got, err = ParseBoxHeader(buf, 0, len(buf))
	if err != nil {
		t.Fatalf("overrun should not be an error: %v", err)
	}
	if got.Valid || got.End != len(buf) {
		t.Fatalf("overrun header: %+v", got)
	}
	if _, err := ParseBoxHeader(buf, 10, len(buf)); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestParseBoxHeaderLargeSize(t *testing.T) {
	buf := make([]byte, 24)
	buf[3] = 1 // size=1 (64-bit)
	copy(buf[4:], "mdat")
	binary.BigEndian.PutUint64(buf[8:], 24)
	got, err := ParseBoxHeader(buf, 0, len(buf))
	if err != nil || !got.Valid || got.Size != 24 || got.HeaderSize != 16 {
		t.Fatalf("64-bit header: %+v err %v", got, err)
	}
}

func TestWalkTopLevelResync(t *testing.T) {
	ftyp := mkBox("ftyp", make([]byte, 8))
	bad := mkBox("mdat", make([]byte, 40))
	binary.BigEndian.PutUint32(bad, 9999) // impossible declared size
	moof := mkBox("moof", make([]byte, 16))
	good := mkBox("mdat", make([]byte, 16))
	buf := append(append(append(append([]byte{}, ftyp...), bad...), moof...), good...)

	boxes, problems := WalkTopLevel(buf, 1<<20)
	if len(boxes) != 3 {
		t.Fatalf("boxes = %+v", boxes)
	}
	if boxes[0].Type != "ftyp" || boxes[1].Type != "moof" || boxes[2].Type != "mdat" {
		t.Fatalf("unexpected box order: %+v", boxes)
	}
	var sawOverrun, sawResync bool
	for _, p := range problems {
		if p.Kind == clock.ErrBoxSizeOverrun {
			sawOverrun = true
		}
		if p.Kind == clock.ErrResync {
			sawResync = true
		}
	}
	if !sawOverrun || !sawResync {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestWalkTopLevelMaxBoxSize(t *testing.T) {
	buf := append(mkBox("mdat", make([]byte, 100)), mkBox("moof", make([]byte, 8))...)
	boxes, problems := WalkTopLevel(buf, 32)
	if len(boxes) != 1 || boxes[0].Type != "moof" {
		t.Fatalf("boxes = %+v", boxes)
	}
	found := false
	for _, p := range problems {
		if p.Kind == clock.ErrBoxSizeOverrun {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected size-overrun problem, got %+v", problems)
	}
}

func TestParseChildrenTruncated(t *testing.T) {
	inner := append(mkBox("mfhd", make([]byte, 8)), mkBox("traf", make([]byte, 4))...)
	// Declare a child size past the parent's end.
	binary.BigEndian.PutUint32(inner[0:], 1000)
	parent := Box{Type: "moof", Offset: 0, HeaderSize: 8, Size: uint64(8 + len(inner)), End: 8 + len(inner), Valid: true}
	buf := append(mkBox("moof", nil)[:8], inner...)
	var problems []Problem
	boxes := ParseChildren(buf, parent, &problems)
	if len(boxes) != 2 {
		t.Fatalf("children = %+v", boxes)
	}
	if len(problems) == 0 || problems[0].Kind != clock.ErrBoxSizeOverrun {
		t.Fatalf("problems = %+v", problems)
	}
}
