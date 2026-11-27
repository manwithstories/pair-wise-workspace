package wav

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"avpipe/internal/media"
)

var rate48 = media.Rate{Num: 48000, Den: 1}

// tone builds a deterministic sine block so fixtures never depend on files.
func tone(frames, channels int, freq float64) []float64 {
	out := make([]float64, frames*channels)
	for i := 0; i < frames; i++ {
		v := 0.5 * math.Sin(2*math.Pi*freq*float64(i)/48000)
		for c := 0; c < channels; c++ {
			out[i*channels+c] = v
		}
	}
	return out
}

func writeFile(t *testing.T, dir, name string, channels int, samples []float64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePCM16(f, rate48, channels, samples); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenAndReadBlocks(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tone.wav", 2, tone(1000, 2, 440))
	r, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info := r.Info()
	if info.Channels != 2 || info.BitDepth != 16 || info.Frames != 1000 {
		t.Fatalf("info=%+v", info)
	}
	var total int
	var blocks int
	for {
		chunk, err := r.ReadBlock("mix", 300, true)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Block.Start != int64(total) {
			t.Fatalf("block %d start %d want %d", blocks, chunk.Block.Start, total)
		}
		if chunk.Partial {
			t.Fatalf("partial block on exact multiple")
		}
		total += chunk.Block.Frames()
		blocks++
	}
	if total != 1000 || blocks != 4 {
		t.Fatalf("total=%d blocks=%d", total, blocks)
	}
	if !r.EOF() {
		t.Fatal("not EOF")
	}
}

func TestStrictRejectsTruncatedFrame(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tone.wav", 1, tone(100, 1, 100))
	// Drop one byte from the data chunk so the final frame is incomplete.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if err := f.Truncate(st.Size() - 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var lastErr error
	for i := 0; i < 100; i++ {
		_, err := r.ReadBlock("mix", 64, true)
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil || errors.Is(lastErr, io.EOF) {
		t.Fatalf("strict read accepted truncated tail: %v", lastErr)
	}
	if !errors.Is(lastErr, media.ErrCorruptInput) {
		t.Fatalf("want corrupt input, got %v", lastErr)
	}
	pe, _ := media.AsError(lastErr)
	if !pe.Recoverable {
		t.Fatalf("truncation should be recoverable: %v", lastErr)
	}
}

func TestRecoveryReportsTruncatedBytes(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tone.wav", 1, tone(100, 1, 100))
	f, _ := os.OpenFile(path, os.O_RDWR, 0o644)
	st, _ := f.Stat()
	_ = f.Truncate(st.Size() - 3)
	f.Close()

	r, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var dropped int64
	for {
		chunk, err := r.ReadBlock("mix", 64, false)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Partial {
			dropped = chunk.DroppedBytes
		}
	}
	if dropped != 3 {
		t.Fatalf("dropped=%d want 3", dropped)
	}
}

// writePCM24 renders 24-bit PCM WAV so the reader's 24-bit path is exercised
// against a real container rather than a synthesized byte slice.
func writePCM24(t *testing.T, path string, rate media.Rate, channels int, samples []float64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := wav.NewEncoder(f, int(rate.Float()), 24, channels, 1)
	buf := &audio.IntBuffer{
		Data:   make([]int, len(samples)),
		Format: &audio.Format{NumChannels: channels, SampleRate: int(rate.Float())},
	}
	for i, s := range samples {
		buf.Data[i] = int(s * 8388607)
	}
	if err := enc.Write(buf); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTwentyFourBitInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tone24.wav")
	samples := tone(400, 2, 440)
	writePCM24(t, path, rate48, 2, samples)
	r, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info := r.Info()
	if info.BitDepth != 24 || info.Channels != 2 || info.Frames != 400 {
		t.Fatalf("info=%+v", info)
	}
	chunk, err := r.ReadBlock("mix", 200, true)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Block.Frames() != 200 {
		t.Fatalf("frames=%d", chunk.Block.Frames())
	}
	// The decoded sample must be within two 24-bit steps of the original
	// (the fixture writer scales by 2^23-1, the reader by 2^23).
	if diff := chunk.Block.Samples[3] - samples[3]; diff > 2.0/8388608 || diff < -2.0/8388608 {
		t.Fatalf("sample decode error: %v vs %v", chunk.Block.Samples[3], samples[3])
	}
	for pos := 200; pos < 400; pos += 200 {
		if _, err := r.ReadBlock("mix", 200, true); err != nil {
			t.Fatal(err)
		}
	}
	if !r.EOF() {
		t.Fatal("not EOF at declared end")
	}
}

func TestLimitsAndFormat(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tone.wav", 2, tone(100, 2, 100))
	lim := DefaultLimits()
	lim.MaxTotalBytes = 100
	if _, err := Open(path, lim); err == nil || media.ExitFor(err) != media.ExitInput {
		t.Fatalf("total byte limit not enforced: %v", err)
	}
	lim = DefaultLimits()
	lim.MaxTracks = 1
	if _, err := Open(path, lim); err == nil {
		t.Fatal("track limit not enforced")
	}
	if _, err := Open(filepath.Join(dir, "missing.wav"), DefaultLimits()); err == nil {
		t.Fatal("missing file opened")
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.wav"), []byte("not a wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "junk.wav"), DefaultLimits()); err == nil {
		t.Fatal("junk accepted")
	}
}

func TestBlockTooLargeRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tone.wav", 1, tone(10, 1, 100))
	r, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.ReadBlock("mix", 0, true); err == nil {
		t.Fatal("zero block size accepted")
	}
}
