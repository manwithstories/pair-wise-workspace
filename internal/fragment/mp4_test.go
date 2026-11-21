package fragment

import (
	"encoding/binary"
	"testing"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func sidxPayload(refs []uint32, declared int) []byte {
	p := make([]byte, 0, 20+12*len(refs))
	p = append(p, 0, 0, 0, 0) // version+flags
	p = append(p, 0, 0, 0, 1) // refID
	p = append(p, 0, 0, 0xbb, 0x80) // timescale 48000
	p = append(p, make([]byte, 8)...) // earliest+offset (v0)
	p = append(p, 0, 0) // reserved
	p = append(p, byte(declared>>8), byte(declared))
	for _, sz := range refs {
		r := make([]byte, 12)
		binary.BigEndian.PutUint32(r, sz)
		binary.BigEndian.PutUint32(r[4:], 96000)
		p = append(p, r...)
	}
	return p
}

func TestParseGood(t *testing.T) {
	data := append([]byte{}, box("ftyp", []byte("isom"))...)
	data = append(data, box("moov", []byte{1, 2})...)
	data = append(data, box("sidx", sidxPayload([]uint32{100, 200}, 2))...)
	data = append(data, box("mdat", []byte{9})...)
	res := Parse("seg.m4s", data)
	if res.Truncated || len(res.Faults) != 0 {
		t.Fatalf("faults: %+v", res.Faults)
	}
	if !res.HasInit {
		t.Fatal("expected init segment")
	}
	if len(res.SidxRefs) != 2 || res.SidxRefs[1].Size != 200 || res.SidxRefs[0].Duration != 96000 {
		t.Fatalf("sidx: %+v", res.SidxRefs)
	}
	if res.Timescale != 48000 {
		t.Fatalf("timescale: %d", res.Timescale)
	}
	if len(res.Boxes) != 4 || res.Boxes[2].Type != "sidx" {
		t.Fatalf("boxes: %+v", res.Boxes)
	}
}

func TestParseTruncatedBox(t *testing.T) {
	data := append([]byte{}, box("ftyp", []byte("isom"))...)
	data = append(data, 0, 0, 0, 100, 'm', 'd', 'a', 't', 1, 2) // 声明 100 实得 10
	res := Parse("t.m4s", data)
	if !res.Truncated || len(res.Boxes) != 1 {
		t.Fatalf("res: %+v", res)
	}
	f := res.Faults[0]
	if f.Offset != int64(len(box("ftyp", []byte("isom")))) || !f.Recoverable {
		t.Fatalf("fault: %+v", f)
	}
}

func TestParseCorruptBoxNoResync(t *testing.T) {
	good := box("free", []byte{1})
	data := append([]byte{}, box("ftyp", []byte("isom"))...)
	data = append(data, 0, 0, 0, 3, 'b', 'a', 'd', '!') // size=3 < 8
	data = append(data, good...)                       // 后续合法 box 不得被识别
	res := Parse("c.m4s", data)
	if len(res.Boxes) != 1 {
		t.Fatalf("must not resync: %+v", res.Boxes)
	}
	if res.Faults[0].Recoverable {
		t.Fatal("corrupt size must be unrecoverable for this segment")
	}
}

func TestParseTruncatedSidx(t *testing.T) {
	data := box("sidx", sidxPayload([]uint32{100}, 3)) // 声明 3 条只有 1 条
	res := Parse("s.m4s", data)
	if !res.Truncated || len(res.SidxRefs) != 1 {
		t.Fatalf("res: %+v", res)
	}
}
