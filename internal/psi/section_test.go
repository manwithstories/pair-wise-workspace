package psi

import (
	"encoding/binary"
	"testing"
)

func testPAT(version uint8, currentNext bool) []byte {
	body := make([]byte, 0, 6)
	body = binary.BigEndian.AppendUint16(body, 1)
	body = binary.BigEndian.AppendUint16(body, 1)
	body = binary.BigEndian.AppendUint16(body, 0xe100)
	length := 5 + len(body) + 4
	raw := []byte{TableIDPAT, 0xb0 | byte(length>>8), byte(length)}
	raw = binary.BigEndian.AppendUint16(raw, 1)
	cn := byte(0)
	if currentNext {
		cn = 1
	}
	raw = append(raw, 0xc0|(version&0x1f)<<1|cn, 0, 0)
	raw = append(raw, body...)
	raw = binary.BigEndian.AppendUint32(raw, CRC32MPEG(raw))
	return raw
}

func TestAssemblerCrossPacketAndCRC(t *testing.T) {
	raw := testPAT(3, true)
	a := NewAssembler()
	first := append([]byte{0}, raw[:10]...)
	secs, errs := a.Push(0, first, true, 1, 0)
	if len(secs) != 0 || len(errs) != 0 {
		t.Fatalf("first push: %d sections %d errors", len(secs), len(errs))
	}
	secs, errs = a.Push(0, raw[10:], false, 2, 188)
	if len(errs) != 0 || len(secs) != 1 {
		t.Fatalf("got %d sections, %d errors", len(secs), len(errs))
	}
	if secs[0].Version != 3 || !secs[0].CurrentNext || secs[0].PacketIndex != 1 {
		t.Fatalf("bad section metadata: %+v", secs[0])
	}

	bad := testPAT(4, true)
	bad[len(bad)-1] ^= 0xff
	secs, errs = a.Push(0, append([]byte{0}, bad...), true, 3, 376)
	if len(secs) != 0 || len(errs) != 1 || a.CRCErrors() != 1 {
		t.Fatalf("CRC rejection failed: sections=%d errs=%d crc=%d", len(secs), len(errs), a.CRCErrors())
	}
}

func TestAssemblerMultipleSections(t *testing.T) {
	a := NewAssembler()
	one := testPAT(1, true)
	two := testPAT(2, true)
	payload := append([]byte{0}, one...)
	payload = append(payload, two...)
	secs, errs := a.Push(0, payload, true, 1, 0)
	if len(errs) != 0 || len(secs) != 2 || secs[0].Version != 1 || secs[1].Version != 2 {
		t.Fatalf("multiple sections: %+v %+v", secs, errs)
	}
}

func TestAssemblerFlushIncomplete(t *testing.T) {
	a := NewAssembler()
	raw := testPAT(5, true)
	if _, errs := a.Push(0, append([]byte{0}, raw[:10]...), true, 9, 0); len(errs) != 0 {
		t.Fatal(errs)
	}
	errs := a.Flush()
	if len(errs) != 1 || a.Incomplete() != 1 {
		t.Fatalf("flush errs=%d incomplete=%d", len(errs), a.Incomplete())
	}
}
