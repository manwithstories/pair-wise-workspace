package riff_test

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"loudness/internal/gen"
	"loudness/internal/riff"
)

func TestUnorderedChunksOddPaddingAndDataBeforeFmt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unordered.wav")
	var b []byte
	b = append(b, "RIFF"...)
	b = append(b, 0, 0, 0, 0, 'W', 'A', 'V', 'E')
	b = appendChunk(b, "JUNK", []byte("abc"))
	samples := []int16{0, 16384, -16384, 32767}
	data := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}
	b = appendChunk(b, "data", data)
	b = appendChunk(b, "fmt ", pcmFmt(1, 48000, 16))
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := riff.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f := d.Format()
	if f.SampleRate != 48000 || f.Channels != 1 || f.BitsPerSample != 16 {
		t.Fatalf("unexpected format: %+v", f)
	}
	dst := make([]float64, 8)
	n, err := d.ReadFrames(dst, 4)
	if n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrames = %d, %v", n, err)
	}
	want := []float64{0, 0.5, -0.5, 32767.0 / 32768}
	for i := range want {
		if math.Abs(dst[i]-want[i]) > 1e-12 {
			t.Fatalf("sample %d = %.12f, want %.12f", i, dst[i], want[i])
		}
	}
}

func TestStreamingDataLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.wav")
	data := make([]byte, 16)
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(i*1000)))
	}
	b := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E'}
	b = appendChunk(b, "fmt ", pcmFmt(1, 48000, 16))
	b = append(b, "data"...)
	b = append(b, 0xff, 0xff, 0xff, 0xff)
	b = append(b, data...)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := riff.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := d.Format().DataBytes; got != 16 {
		t.Fatalf("data bytes = %d, want 16", got)
	}
	dst := make([]float64, 8)
	if n, _ := d.ReadFrames(dst, 8); n != 8 {
		t.Fatalf("frames = %d, want 8", n)
	}
}

func TestOddSizedDataChunkPadding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd-data.wav")
	b := []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E'}
	b = appendChunk(b, "data", []byte{0, 128, 255})
	b = appendChunk(b, "fmt ", pcmFmt(1, 48000, 8))
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := riff.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dst := make([]float64, 3)
	if n, err := d.ReadFrames(dst, 3); n != 3 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrames = %d, %v", n, err)
	}
	if dst[0] != -1 || dst[1] != 0 || math.Abs(dst[2]-127.0/128) > 1e-12 {
		t.Fatalf("samples = %v", dst)
	}
}

func TestExtensibleSurroundAndNaN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "surround.wav")
	err := gen.WriteWAV(path, gen.WAVSpec{
		SampleRate: 96000, Channels: 6, Bits: 32, Float: true,
		Extensible: true, ChannelMask: 0x3f, Seconds: 0.01,
	}, func(frame, ch int) float64 {
		if frame == 1 && ch == 2 {
			return math.NaN()
		}
		return 0.25
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := riff.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	gains, err := d.Format().ChannelGains()
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{1, 1, 1, 0, 1.41, 1.41}
	for i := range want {
		if gains[i] != want[i] {
			t.Fatalf("gain %d = %g, want %g", i, gains[i], want[i])
		}
	}
	dst := make([]float64, 6*960)
	var frames int
	for {
		n, err := d.ReadFrames(dst, 960)
		frames += n
		if err != nil {
			break
		}
	}
	if frames != 960 || d.NaNs() != 1 {
		t.Fatalf("frames=%d nans=%d, want 960/1", frames, d.NaNs())
	}
}

func TestUnknownSurroundMask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-mask.wav")
	err := gen.WriteWAV(path, gen.WAVSpec{
		SampleRate: 48000, Channels: 6, Bits: 16,
		Extensible: true, ChannelMask: 0x3e, Seconds: 0.01,
	}, func(frame, ch int) float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	d, err := riff.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, err = d.Format().ChannelGains()
	if !errors.Is(err, riff.ErrUnsupportedLayout) {
		t.Fatalf("error = %v, want unsupported_layout", err)
	}
}

func pcmFmt(channels int, rate int, bits int) []byte {
	b := make([]byte, 16)
	blockAlign := channels * bits / 8
	binary.LittleEndian.PutUint16(b[0:], 1)
	binary.LittleEndian.PutUint16(b[2:], uint16(channels))
	binary.LittleEndian.PutUint32(b[4:], uint32(rate))
	binary.LittleEndian.PutUint32(b[8:], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(b[12:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(b[14:], uint16(bits))
	return b
}

func appendChunk(dst []byte, id string, body []byte) []byte {
	dst = append(dst, id...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)))
	dst = append(dst, size[:]...)
	dst = append(dst, body...)
	if len(body)%2 == 1 {
		dst = append(dst, 0)
	}
	return dst
}
