package loudness

import (
	"bytes"
	"math"
	"testing"

	"loudcheck/internal/wavio"
)

type memSink struct {
	buf []byte
	off int64
}

func (m *memSink) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if int(end) > len(m.buf) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.off:], p)
	m.off = end
	return len(p), nil
}
func (m *memSink) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.off = off
	case 1:
		m.off += off
	case 2:
		m.off = int64(len(m.buf)) + off
	}
	return m.off, nil
}
func (m *memSink) Close() error { return nil }

func measure(t *testing.T, hdr wavio.WriteHeader, planar [][]float64) EngineResult {
	t.Helper()
	file := &memSink{}
	writer, err := wavio.NewWriter(file, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrames(planar); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	result, _, err := MeasureStream(bytes.NewReader(file.buf), 8192)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSilenceIsUndefined(t *testing.T) {
	hdr := wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 16}
	planar := [][]float64{make([]float64, 48000)}
	result := measure(t, hdr, planar)
	if result.Defined || !math.IsNaN(result.IntegratedLUFS) {
		t.Fatalf("silence must be undefined, got defined=%v lufs=%v", result.Defined, result.IntegratedLUFS)
	}
	if result.BlocksAbsolute != 0 {
		t.Fatalf("silence must pass no absolute gate, got %d", result.BlocksAbsolute)
	}
}

func TestBlockCountAndGate(t *testing.T) {
	hdr := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	planar := wavio.TonePlanar(48000, 20, 2, 1000, 0.1, 0.3)
	result := measure(t, hdr, planar)
	if result.BlocksTotal != 197 {
		t.Fatalf("blocks total %d, want 197", result.BlocksTotal)
	}
	if result.BlocksAbsolute != result.BlocksTotal {
		t.Fatalf("all loud blocks should pass absolute gate")
	}
	if math.Abs(result.RelativeGateLUFS-(result.IntegratedLUFS-10)) > 0.05 {
		t.Fatalf("relative gate %.3f not 10 LU below integrated %.3f",
			result.RelativeGateLUFS, result.IntegratedLUFS)
	}
}

func TestMonoAndStereoDoNotShareWeights(t *testing.T) {
	stereo := measure(t, wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24},
		wavio.TonePlanar(48000, 10, 2, 1000, 0.1, 0.3))
	mono := measure(t, wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 16},
		wavio.TonePlanar(48000, 10, 1, 1000, 0.1, 0.3))
	if math.Abs(stereo.IntegratedLUFS-mono.IntegratedLUFS-3.01) > 0.05 {
		t.Fatalf("stereo %.3f should be ~3.01 LU above mono %.3f",
			stereo.IntegratedLUFS, mono.IntegratedLUFS)
	}
}

func TestDeterministicRepeatedMeasure(t *testing.T) {
	hdr := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	planar := wavio.TonePlanar(48000, 8, 2, 1000, 0.2, 0.7)
	first := measure(t, hdr, planar)
	second := measure(t, hdr, planar)
	if first.IntegratedLUFS != second.IntegratedLUFS || first.TruePeakDBTP != second.TruePeakDBTP {
		t.Fatalf("non-deterministic results: %+v vs %+v", first, second)
	}
}

func TestShortTermPartialFlag(t *testing.T) {
	hdr := wavio.WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 16}
	planar := wavio.TonePlanar(48000, 2, 1, 1000, 0.2, 0.1)
	result := measure(t, hdr, planar)
	if !result.ShortTerm.Partial {
		t.Fatal("sub-3s program must be flagged partial")
	}
}
