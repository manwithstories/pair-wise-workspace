package garc

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

func TestReaderBoundaryAndOverflow(t *testing.T) {
	r, err := NewReader(make([]byte, 10))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	r.SetOrder(binary.LittleEndian)

	if err := r.CheckRange(8, 2); err != nil {
		t.Errorf("in-range read rejected: %v", err)
	}
	err = r.CheckRange(8, 3)
	if err == nil {
		t.Fatal("out-of-range read accepted")
	}
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != "OUT_OF_BOUNDS" {
		t.Fatalf("want OUT_OF_BOUNDS, got %v", err)
	}
	// uint32 加法溢出防护。
	if _, ok := CheckAddU32(0xFFFFFFFF, 1); ok {
		t.Error("overflow not detected")
	}
	if sum, ok := CheckAddU32(0xFFFFFFFE, 1); !ok || sum != 0xFFFFFFFF {
		t.Error("valid addition mis-flagged")
	}
}

func TestReaderSizeLimit(t *testing.T) {
	if _, err := NewReader(make([]byte, MaxFileSize+1)); err == nil {
		t.Fatal("file over 512 MiB accepted")
	}
}

func TestVarint(t *testing.T) {
	// 手工构造 LEB128：300 = 0xAC 0x02。
	data := []byte{0xAC, 0x02, 0x00}
	r, _ := NewReader(data)
	v, n, err := r.Varint(0)
	if err != nil || v != 300 || n != 2 {
		t.Fatalf("varint 300: v=%d n=%d err=%v", v, n, err)
	}
	// 永不终止（5 个延续位）→ VARINT_OVERFLOW。
	bad := []byte{0x80, 0x80, 0x80, 0x80, 0x80}
	rb, _ := NewReader(bad)
	if _, _, err := rb.Varint(0); err == nil {
		t.Fatal("unterminated varint accepted")
	}
	// 第 5 字节携带超 uint32 位 → OVERFLOW。
	wide := []byte{0x80, 0x80, 0x80, 0x80, 0x10}
	rw, _ := NewReader(wide)
	if _, _, err := rw.Varint(0); err == nil {
		t.Fatal("uint32-overflow varint accepted")
	}
	// 截断 → TRUNCATED_RECORD。
	trunc := []byte{0x80}
	rt, _ := NewReader(trunc)
	if _, _, err := rt.Varint(0); err == nil {
		t.Fatal("truncated varint accepted")
	}
}

func TestCRCMismatch(t *testing.T) {
	r, _ := NewReader([]byte{1, 2, 3, 4})
	if err := r.CheckCRC(0, 4, 0xDEADBEEF); err == nil {
		t.Fatal("bad CRC accepted")
	}
	if err := r.CheckCRC(0, 4, crc32.ChecksumIEEE([]byte{1, 2, 3, 4})); err != nil {
		t.Errorf("good CRC rejected: %v", err)
	}
}
