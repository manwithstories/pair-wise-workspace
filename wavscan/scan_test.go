package wavscan_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/core"
	"archivectl/internal/fixture"
	iwav "archivectl/internal/wavscan"
)

func TestScanValidPCMAndCueChunks(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(fixture.Config{
		Dir: dir, Seed: 3, Frames: 16384, Channels: 2, Bits: 16,
		Injections: []string{"vfr"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := iwav.ScanPath(m.NormalWAV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Format.Channels != 2 || rep.Format.BitsPerSample != 16 {
		t.Fatalf("unexpected format: %+v", rep.Format)
	}
	if rep.DeclaredFrames != 16384 || rep.AvailableFrames != 16384 {
		t.Fatalf("frame counts declared=%d available=%d", rep.DeclaredFrames, rep.AvailableFrames)
	}
	if len(rep.Cues) != m.ExpectedKeyframes {
		t.Fatalf("cues=%d want %d", len(rep.Cues), m.ExpectedKeyframes)
	}
	var unknown, padded bool
	for _, c := range rep.Chunks {
		if c.Unknown {
			unknown = true
			if c.OddPadding && c.PaddingPresent {
				padded = true
			}
		}
	}
	if !unknown || !padded {
		t.Fatalf("unknown/padding not marked: unknown=%v padded=%v chunks=%+v", unknown, padded, rep.Chunks)
	}
}

func TestScanRejectsUnsupportedChannelsAndMalformedFmt(t *testing.T) {
	dir := t.TempDir()
	badChannels := filepath.Join(dir, "channels.wav")
	writeMinimalWAV(t, badChannels, 9, 16, 16)
	_, err := iwav.ScanPath(badChannels)
	if !core.AsKind(err, core.KindUnsupported) {
		t.Fatalf("expected unsupported format, got %v", err)
	}

	shortFmt := filepath.Join(dir, "short-fmt.wav")
	writeRIFFWithFmt(t, shortFmt, []byte{1, 0, 1, 0})
	_, err = iwav.ScanPath(shortFmt)
	if !core.AsKind(err, core.KindMalformedFMT) {
		t.Fatalf("expected malformed fmt, got %v", err)
	}
}

func TestScanDetectsMissingOddPadding(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(fixture.Config{
		Dir: dir, Seed: 4, Frames: 8192, Channels: 1, Bits: 16,
		Injections: []string{"missing-padding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = iwav.ScanPath(m.DamagedWAV)
	if !core.AsKind(err, core.KindPadding) {
		t.Fatalf("expected padding error, got %v", err)
	}
	if m.ScanError == "" {
		t.Fatal("generator should record padding scan failure")
	}
}

func TestScanChecksDataAlignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "misaligned.wav")
	// mono 16-bit has a 2-byte frame; three data bytes is deliberately odd.
	writeMinimalWAV(t, path, 1, 16, 3)
	_, err := iwav.ScanPath(path)
	if !core.AsKind(err, core.KindAlignment) {
		t.Fatalf("expected alignment error, got %v", err)
	}
}

func TestScanAcceptsAndReportsDuplicateCue(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(fixture.Config{
		Dir: dir, Seed: 5, Frames: 8192, Channels: 1, Bits: 16,
		Injections: []string{"duplicate-cue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := iwav.ScanPath(m.DamagedWAV)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cues) != m.ExpectedKeyframes+1 {
		t.Fatalf("cues=%d want original+duplicate=%d", len(rep.Cues), m.ExpectedKeyframes+1)
	}
	unique := map[int64]bool{}
	for _, c := range rep.Cues {
		unique[c.Frame] = true
	}
	if len(unique) != m.ExpectedKeyframes {
		t.Fatalf("unique cue frames=%d want %d", len(unique), m.ExpectedKeyframes)
	}
}

func TestFormatValidationAcceptsAllDocumentedPCMEnvelopes(t *testing.T) {
	for channels := 1; channels <= 8; channels++ {
		for _, bits := range []int{8, 16, 24, 32} {
			frameBytes := channels * bits / 8
			f := iwav.Format{
				AudioFormat: 1, Channels: channels, SampleRate: 48000,
				ByteRate: int64(48000 * frameBytes), BlockAlign: frameBytes, BitsPerSample: bits,
			}
			if err := f.Validate("test"); err != nil {
				t.Fatalf("channels=%d bits=%d should be supported: %v", channels, bits, err)
			}
		}
	}
}

func writeMinimalWAV(t *testing.T, path string, channels, bits, dataBytes int) {
	t.Helper()
	frameBytes := channels * bits / 8
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:2], 1)
	binary.LittleEndian.PutUint16(fmtBody[2:4], uint16(channels))
	binary.LittleEndian.PutUint32(fmtBody[4:8], 48000)
	binary.LittleEndian.PutUint32(fmtBody[8:12], uint32(48000*frameBytes))
	binary.LittleEndian.PutUint16(fmtBody[12:14], uint16(frameBytes))
	binary.LittleEndian.PutUint16(fmtBody[14:16], uint16(bits))
	writeRIFFWithFmtAndData(t, path, fmtBody, make([]byte, dataBytes))
}

func writeRIFFWithFmt(t *testing.T, path string, fmtBody []byte) {
	t.Helper()
	writeRIFFWithFmtAndData(t, path, fmtBody, []byte{0, 0})
}

func writeRIFFWithFmtAndData(t *testing.T, path string, fmtBody, data []byte) {
	t.Helper()
	var b []byte
	put := func(v any) {
		var buf [4]byte
		switch x := v.(type) {
		case string:
			b = append(b, x...)
		case uint16:
			binary.LittleEndian.PutUint16(buf[:2], x)
			b = append(b, buf[:2]...)
		case uint32:
			binary.LittleEndian.PutUint32(buf[:4], x)
			b = append(b, buf[:4]...)
		}
	}
	pad := len(data) % 2
	riffSize := uint32(4 + 8 + len(fmtBody) + 8 + len(data) + pad)
	put("RIFF")
	put(riffSize)
	put("WAVE")
	put("fmt ")
	put(uint32(len(fmtBody)))
	b = append(b, fmtBody...)
	put("data")
	put(uint32(len(data)))
	b = append(b, data...)
	if pad == 1 {
		b = append(b, 0)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
