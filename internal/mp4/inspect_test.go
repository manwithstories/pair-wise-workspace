package mp4

import (
	"testing"

	"mediaroute/internal/model"
)

func seg(payloads ...[]byte) []byte {
	var b []byte
	b = AppendBox(b, "styp", []byte("msdh"))
	for _, p := range payloads {
		b = AppendBox(b, "moof", p[:len(p)/2])
		b = AppendBox(b, "mdat", p[len(p)/2:])
	}
	return b
}

func TestInspectGood(t *testing.T) {
	data := seg([]byte("hello world"), []byte("foobar!!"))
	if err := Inspect(0, data); err != nil {
		t.Fatal(err)
	}
	boxes, err := Boxes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 5 || boxes[0].Type != "styp" || boxes[1].Type != "moof" {
		t.Fatalf("boxes: %+v", boxes)
	}
	var end uint64
	for _, b := range boxes {
		end = b.Offset + b.Size
	}
	if end != uint64(len(data)) {
		t.Fatalf("boxes end %d != len %d", end, len(data))
	}
}

func TestInspectTruncated(t *testing.T) {
	data := seg([]byte("hello world"))
	trunc := data[:len(data)-3] // cut inside the mdat
	err := Inspect(4, trunc)
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrCorrupt {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if me.Seq != 4 {
		t.Fatalf("segment context lost: %+v", me)
	}
	off := CorruptOffset(err)
	if off < 0 || off >= int64(len(data)) {
		t.Fatalf("offset %d out of range", off)
	}
	// the mdat box starts exactly where inspection stopped
	if got := string(trunc[off+4 : off+8]); got != "mdat" {
		t.Fatalf("offset %d does not point at mdat header (%q)", off, got)
	}
	if model.ExitCode(me) != model.ExitCorrupt {
		t.Fatalf("exit code %d", model.ExitCode(me))
	}
}

func TestInspectTrailingGarbage(t *testing.T) {
	data := append(seg([]byte("abcdefgh")), 0x1, 0x2, 0x3)
	if err := Inspect(1, data); err == nil {
		t.Fatal("trailing bytes must be rejected")
	}
}
