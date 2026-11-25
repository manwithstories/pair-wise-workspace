package wav

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"mediaaudit/internal/model"
)

// buildWAV writes a minimal PCM WAV with a trailing crCC chunk for the given
// interleaved samples. data is the raw PCM payload.
func buildWAV(t *testing.T, path string, rate uint32, ch, bits uint16, data []byte, crc []uint32) {
	t.Helper()
	var b []byte
	put := func(p []byte, v uint32) { binary.LittleEndian.PutUint32(p, v) }
	// RIFF header
	riffSize := 4 + (8+16) + (8+len(data)+len(data)%2)
	if len(crc) > 0 {
		riffSize += 8 + len(crc)*4
	}
	head := make([]byte, 12)
	copy(head[0:4], "RIFF")
	put(head[4:], uint32(riffSize))
	copy(head[8:], "WAVE")
	b = append(b, head...)
	fmtChunk := make([]byte, 8+16)
	copy(fmtChunk[0:4], "fmt ")
	put(fmtChunk[4:], 16)
	binary.LittleEndian.PutUint16(fmtChunk[8:], 1)
	binary.LittleEndian.PutUint16(fmtChunk[10:], ch)
	put(fmtChunk[12:], rate)
	put(fmtChunk[16:], rate*uint32(ch)*uint32(bits)/8)
	binary.LittleEndian.PutUint16(fmtChunk[20:], uint16(ch)*bits/8)
	binary.LittleEndian.PutUint16(fmtChunk[22:], bits)
	b = append(b, fmtChunk...)
	dataHead := make([]byte, 8)
	copy(dataHead[0:4], "data")
	put(dataHead[4:], uint32(len(data)))
	b = append(b, dataHead...)
	b = append(b, data...)
	if len(data)%2 == 1 {
		b = append(b, 0)
	}
	if len(crc) > 0 {
		cb := make([]byte, 8+4*len(crc))
		copy(cb[0:4], CRCChunkID)
		put(cb[4:], uint32(4*len(crc)))
		for i, c := range crc {
			put(cb[8+i*4:], c)
		}
		b = append(b, cb...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func pcmTiles(frames, ch, bits int, fill byte) []byte {
	fb := ch * bits / 8
	buf := make([]byte, frames*fb)
	for i := range buf {
		buf[i] = fill
	}
	return buf
}

func TestParseAndCRCHappy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.wav")
	frames := TileFrames * 4
	data := pcmTiles(frames, 2, 16, 0xAB)
	tb := TileFrames * 2 * 2
	crcs := make([]uint32, frames/TileFrames)
	for i := range crcs {
		crcs[i] = crc32.Checksum(data[i*tb:(i+1)*tb], crcTable)
	}
	buildWAV(t, p, 48000, 2, 16, data, crcs)
	rd, err := NewReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	h := rd.Header()
	if h.Channels != 2 || h.Bits != 16 || h.SampleRate != 48000 {
		t.Fatalf("header mismatch: %+v", h)
	}
	if h.FrameCount != int64(frames) {
		t.Fatalf("frames=%d want %d", h.FrameCount, frames)
	}
	rep, err := rd.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Faults.Count() != 0 {
		t.Fatalf("unexpected faults: %+v", rep.Faults.All())
	}
}

func TestCRCFlipIsolatesTile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "flip.wav")
	frames := TileFrames * 4
	data := pcmTiles(frames, 6, 24, 0x55)
	tb := TileFrames * 6 * 3
	crcs := make([]uint32, frames/TileFrames)
	for i := range crcs {
		crcs[i] = crc32.Checksum(data[i*tb:(i+1)*tb], crcTable)
	}
	// Corrupt the first tile only; later tiles must stay clean.
	data[10] ^= 0xFF
	buildWAV(t, p, 48000, 6, 24, data, crcs)
	rd, _ := NewReader(p)
	defer rd.Close()
	rep, err := rd.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Faults.Count() != 1 {
		t.Fatalf("want 1 CRC fault, got %d", rep.Faults.Count())
	}
	f := rep.Faults.All()[0]
	if f.Kind != model.KindCRC || f.Range.First != 0 || f.Range.Last != TileFrames {
		t.Fatalf("wrong fault: %+v", f)
	}
	if f.ByteOffset != rd.Header().DataChunk.Offset {
		t.Fatalf("byte offset = %d", f.ByteOffset)
	}
	dmg := rep.DamagedRanges()
	if len(dmg) != 1 || dmg[0].Len() != TileFrames {
		t.Fatalf("damaged ranges wrong: %+v", dmg)
	}
}

func TestTruncatedTailIsResidual(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trunc.wav")
	// 2 full tiles + 7 residual frames of stereo 16-bit PCM.
	frames := TileFrames*2 + 7
	data := pcmTiles(frames, 2, 16, 0x10)
	buildWAV(t, p, 48000, 2, 16, data, nil)
	// Truncate the file by 3 bytes (inside the trailing stereo frame).
	st, _ := os.Stat(p)
	if err := os.Truncate(p, st.Size()-3); err != nil {
		t.Fatal(err)
	}
	// The reader must NOT fail the whole track; it records a tail fault and
	// exposes the intact prefix as usable frames.
	rd, err := NewReader(p)
	if err != nil {
		t.Fatalf("short tail should be isolated, not fatal: %v", err)
	}
	defer rd.Close()
	if rd.TailFault() == nil || rd.TailFault().Kind != model.KindTruncated {
		t.Fatalf("expected a located truncated tail fault, got %+v", rd.TailFault())
	}
	// Full frames before the truncation remain addressable.
	if rd.Header().FrameCount < TileFrames*2 {
		t.Fatalf("intact prefix frames=%d want >= %d", rd.Header().FrameCount, TileFrames*2)
	}
	rep, err := rd.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	// The two complete tiles must still be structurally sound.
	if n := len(rep.Tiles); n < 2 {
		t.Fatalf("expected intact tiles, got %d", n)
	}
}

func TestTruncatedDataOnlyNoCrCC(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "truncdata.wav")
	// No crCC chunk; truncate inside the data payload itself.
	frames := TileFrames * 3
	data := pcmTiles(frames, 2, 16, 0x20)
	buildWAV(t, p, 48000, 2, 16, data, nil)
	st, _ := os.Stat(p)
	if err := os.Truncate(p, st.Size()-5); err != nil {
		t.Fatal(err)
	}
	rd, err := NewReader(p)
	if err != nil {
		t.Fatalf("data short tail should be isolated: %v", err)
	}
	defer rd.Close()
	if rd.TailFault() == nil {
		t.Fatal("expected truncated tail fault")
	}
	if _, err := rd.Inspect(); err != nil {
		t.Fatalf("inspect should tolerate the residual: %v", err)
	}
}

func TestRejectsUnsupportedDepth(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.wav")
	buildWAV(t, p, 48000, 2, 8, pcmTiles(8, 2, 8, 1), nil)
	rd, err := NewReader(p)
	if err == nil {
		rd.Close()
		t.Fatal("expected rejection of 8-bit PCM")
	}
}
