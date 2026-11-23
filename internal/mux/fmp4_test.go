package mux_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/mux"
)

var tb = clock.Timebase{SampleRate: 48000, Channels: 2, BitsPerSample: 16}

// memFile 是实现 io.ReadWriteSeeker 的内存文件，供 box 写入与回读使用。
type memFile struct {
	b   []byte
	pos int64
}

func (m *memFile) Write(p []byte) (int, error) {
	end := int(m.pos) + len(p)
	if end > len(m.b) {
		if cap(m.b) >= end {
			m.b = m.b[:end] // 回退写入后再次前进：扩展长度但保留底层数组
		} else {
			grown := make([]byte, end)
			copy(grown, m.b)
			m.b = grown
		}
	}
	copy(m.b[m.pos:end], p)
	m.pos = int64(end)
	return len(p), nil
}
func (m *memFile) Read(p []byte) (int, error) {
	if int(m.pos) >= len(m.b) {
		return 0, errors.New("EOF")
	}
	n := copy(p, m.b[m.pos:])
	m.pos += int64(n)
	return n, nil
}
func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	var p int64
	switch whence {
	case 0:
		p = offset
	case 1:
		p = m.pos + offset
	case 2:
		p = int64(len(m.b)) + offset
	}
	if p < 0 {
		return 0, errors.New("negative seek")
	}
	m.pos = p
	return p, nil
}
func (m *memFile) bytes() []byte { return append([]byte(nil), m.b...) }

func pcm(frames int) []byte {
	buf := make([]byte, frames*tb.FrameSize())
	for i := range buf {
		buf[i] = byte(i%251 + 7)
	}
	return buf
}

func TestInitSegmentStructure(t *testing.T) {
	m, err := mux.New(tb, 1)
	if err != nil {
		t.Fatal(err)
	}
	f := &memFile{}
	if err := m.WriteInit(f); err != nil {
		t.Fatalf("write init: %v", err)
	}
	raw := f.bytes()
	if len(raw) < 16 || string(raw[4:8]) != "ftyp" {
		t.Fatal("init segment missing ftyp box")
	}
	if !contains(raw, "moov") || !contains(raw, "ipcm") || !contains(raw, "pcmC") {
		t.Fatal("init segment missing moov/ipcm/pcmC")
	}
	if !contains(raw, "trex") {
		t.Fatal("init segment missing mvex/trex for fragmented output")
	}
}

func contains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}

func TestSegmentRoundTripAndChecksum(t *testing.T) {
	m, err := mux.New(tb, 1)
	if err != nil {
		t.Fatal(err)
	}
	src := pcm(25)
	f := &memFile{}
	if err := m.WriteSegment(f, 1, 100, src); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	raw := f.bytes()
	for _, want := range []string{"styp", "moof", "tfdt", "trun", "mdat"} {
		if !contains(raw, want) {
			t.Fatalf("segment missing %s box", want)
		}
	}
	// 回读核验：结构完整且 mdat 载荷与源 PCM 逐字节一致。
	sum, err := mux.VerifyReadback(bytes.NewReader(raw), src)
	if err != nil {
		t.Fatalf("readback verify: %v", err)
	}
	if want := sha256.Sum256(src); sum != want {
		t.Fatal("readback checksum differs from source PCM sha256")
	}
}

func TestDeterministicSegmentBytes(t *testing.T) {
	m, _ := mux.New(tb, 1)
	src := pcm(10)
	a, b := &memFile{}, &memFile{}
	if err := m.WriteSegment(a, 3, 50, src); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteSegment(b, 3, 50, src); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.bytes(), b.bytes()) {
		t.Fatal("identical segment inputs produced different fMP4 bytes")
	}
}

func TestTrunDataOffsetPointsIntoMdat(t *testing.T) {
	m, _ := mux.New(tb, 1)
	src := pcm(16)
	f := &memFile{}
	if err := m.WriteSegment(f, 1, 0, src); err != nil {
		t.Fatal(err)
	}
	// 篡改 mdat 载荷首字节后，回读核验必须失败（证明确实比对了内容）。
	raw := f.bytes()
	mdatPos := bytes.Index(raw, []byte("mdat"))
	if mdatPos < 0 {
		t.Fatal("mdat not found")
	}
	raw[mdatPos+8] ^= 0xFF
	if _, err := mux.VerifyReadback(bytes.NewReader(raw), src); err == nil {
		t.Fatal("tampered mdat payload passed readback verification")
	}
}

func TestRejectsInvalidTimebase(t *testing.T) {
	if _, err := mux.New(clock.Timebase{SampleRate: 0}, 1); err == nil {
		t.Fatal("invalid timebase should be rejected")
	}
}
