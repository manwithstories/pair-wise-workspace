package wav

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildRF64 writes a minimal RF64 file: RF64 tag, a ds64 chunk carrying the
// real 64-bit data size, then fmt and a data chunk whose 32-bit size is the
// 0xFFFFFFFF sentinel.
func buildRF64(t *testing.T, path string, rate uint32, ch, bits uint16, data []byte) {
	t.Helper()
	frameBytes := int(ch) * int(bits) / 8
	var b []byte

	head := make([]byte, 12)
	copy(head[0:4], "RF64")
	binary.LittleEndian.PutUint32(head[4:], 0xFFFFFFFF) // placeholder
	copy(head[8:], "WAVE")
	b = append(b, head...)

	// ds64 payload (after its 8-byte header): RIFF size, data size, sample
	// count (three uint64) then table count (uint32) = 28 bytes.
	ds := make([]byte, 8+28)
	copy(ds[0:4], "ds64")
	binary.LittleEndian.PutUint32(ds[4:], 28)
	binary.LittleEndian.PutUint64(ds[8:], uint64(len(data))+64)     // RIFF size
	binary.LittleEndian.PutUint64(ds[16:], uint64(len(data)))      // data size
	binary.LittleEndian.PutUint64(ds[24:], uint64(len(data)/frameBytes)) // sample count
	b = append(b, ds...)

	fmtc := make([]byte, 8+16)
	copy(fmtc[0:4], "fmt ")
	binary.LittleEndian.PutUint32(fmtc[4:], 16)
	binary.LittleEndian.PutUint16(fmtc[8:], 1)
	binary.LittleEndian.PutUint16(fmtc[10:], ch)
	binary.LittleEndian.PutUint32(fmtc[12:], rate)
	binary.LittleEndian.PutUint32(fmtc[16:], rate*uint32(frameBytes))
	binary.LittleEndian.PutUint16(fmtc[20:], uint16(frameBytes))
	binary.LittleEndian.PutUint16(fmtc[22:], bits)
	b = append(b, fmtc...)

	dh := make([]byte, 8)
	copy(dh[0:4], "data")
	binary.LittleEndian.PutUint32(dh[4:], 0xFFFFFFFF) // RF64 sentinel
	b = append(b, dh...)
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRF64ReadsRealSizeFromDS64(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rf64.wav")
	frames := TileFrames * 2
	data := pcmTiles(frames, 2, 16, 0x42)
	buildRF64(t, p, 48000, 2, 16, data)
	rd, err := NewReader(p)
	if err != nil {
		t.Fatalf("RF64 should parse: %v", err)
	}
	defer rd.Close()
	if rd.Header().FrameCount != int64(frames) {
		t.Fatalf("RF64 frames=%d want %d (ds64 size not applied)",
			rd.Header().FrameCount, frames)
	}
	if rd.Header().DataChunk.Size != int64(len(data)) {
		t.Fatalf("data size=%d want %d", rd.Header().DataChunk.Size, len(data))
	}
}

func TestBitDepths16And32(t *testing.T) {
	for _, bits := range []uint16{16, 32} {
		dir := t.TempDir()
		p := filepath.Join(dir, "depth.wav")
		frames := TileFrames
		data := pcmTiles(frames, 2, int(bits), 0x9)
		buildWAV(t, p, 48000, 2, bits, data, nil)
		rd, err := NewReader(p)
		if err != nil {
			t.Fatalf("%d-bit parse failed: %v", bits, err)
		}
		if rd.Header().Bits != bits {
			t.Fatalf("bits=%d want %d", rd.Header().Bits, bits)
		}
		rep, err := rd.Inspect()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Faults.Count() != 0 {
			t.Fatalf("%d-bit clean file had faults: %+v", bits, rep.Faults.All())
		}
		rd.Close()
	}
}

func TestThirtyTwoChannelsAccepted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ch32.wav")
	frames := TileFrames
	data := pcmTiles(frames, MaxChannels, 16, 0x5)
	buildWAV(t, p, 48000, MaxChannels, 16, data, nil)
	rd, err := NewReader(p)
	if err != nil {
		t.Fatalf("32 channels should be accepted: %v", err)
	}
	defer rd.Close()
	if rd.Header().Channels != MaxChannels {
		t.Fatalf("channels=%d want %d", rd.Header().Channels, MaxChannels)
	}
	if rd.Header().FrameBytes != int64(MaxChannels)*2 {
		t.Fatalf("frame bytes=%d", rd.Header().FrameBytes)
	}
}
