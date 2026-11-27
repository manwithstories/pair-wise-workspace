package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"fieldcut/internal/media"
)

func pcm16(t *testing.T, values []float64) []byte {
	t.Helper()
	out := make([]byte, len(values)*2)
	for i, v := range values {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(v*32767))))
	}
	return out
}

func TestParsePCM(t *testing.T) {
	data := pcm16(t, []float64{0, 0.5, -0.5, 1.0})
	got, err := ParsePCM(data, 1)
	if err != nil {
		t.Fatalf("ParsePCM: %v", err)
	}
	if len(got) != 4 || math.Abs(got[1]-0.5) > 0.001 || got[3] < 0.99 {
		t.Fatalf("samples = %v", got)
	}
	if _, err := ParsePCM([]byte{1}, 1); err == nil {
		t.Fatal("odd buffer accepted")
	}
	if _, err := ParsePCM(data, 3); err == nil {
		t.Fatal("3 channels accepted")
	}
}

func TestDetectPulsesFindsBursts(t *testing.T) {
	buf := make([]float64, 4800)
	buf[100], buf[101] = 0.9, 0.8
	buf[1300], buf[1301] = 0.7, 0.95
	buf[2500] = 0.6
	pulses, err := DetectPulses(buf, 0.3, 64)
	if err != nil {
		t.Fatalf("DetectPulses: %v", err)
	}
	if len(pulses) != 3 {
		t.Fatalf("pulses = %d, want 3", len(pulses))
	}
	if pulses[0].Peak != 100 || pulses[1].Peak != 1301 || pulses[2].Peak != 2500 {
		t.Fatalf("peaks = %d,%d,%d", pulses[0].Peak, pulses[1].Peak, pulses[2].Peak)
	}
}

func TestProbeAlignmentExactGrid(t *testing.T) {
	const rate = media.AudioRate
	expected := int64(0)
	data, err := SyntheticPCM(rate*2, rate, 0, rate/2, 64, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := ParsePCM(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ProbeAlignment(samples, rate, expected, 0.3, 64)
	if err != nil {
		t.Fatalf("ProbeAlignment: %v", err)
	}
	if len(p.Pulses) != 4 {
		t.Fatalf("pulses = %d, want 4", len(p.Pulses))
	}
	if math.Abs(p.OffsetMs()) > 0.001 {
		t.Fatalf("offset = %.4f ms, want ~0", p.OffsetMs())
	}
	if p.MaxErrorNanos > 1000 {
		t.Fatalf("grid error = %d ns", p.MaxErrorNanos)
	}
	if !p.Aligned() {
		t.Fatal("expected aligned probe")
	}
}

func TestProbeAlignmentReportsSkew(t *testing.T) {
	const rate = media.AudioRate
	data, err := SyntheticPCM(rate, rate, rate/4, rate/4, 48, 0.7)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := ParsePCM(data, 1)
	// Pulse 0 sits at sample 12000 = 250 ms; claim it belongs at 10 ms.
	p, err := ProbeAlignment(samples, rate, 10_000_000, 0.25, 32)
	if err != nil {
		t.Fatalf("ProbeAlignment: %v", err)
	}
	if math.Abs(p.OffsetMs()-240.0) > 0.1 {
		t.Fatalf("offset = %.3f ms", p.OffsetMs())
	}
	if p.Aligned() {
		t.Fatal("skewed probe reported aligned")
	}
}

func TestProbeAlignmentNeedsTwoPulses(t *testing.T) {
	buf := make([]float64, 2048)
	buf[10] = 0.9
	_, err := ProbeAlignment(buf, media.AudioRate, 0, 0.3, 64)
	if err == nil {
		t.Fatal("single pulse accepted")
	}
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindInput {
		t.Fatalf("not input error: %v", err)
	}
}

func TestSyntheticPCMRejectsBadGeometry(t *testing.T) {
	if _, err := SyntheticPCM(100, 48000, 0, 0, 8, 0.5); err == nil {
		t.Fatal("zero period accepted")
	}
	if _, err := SyntheticPCM(100, 48000, 200, 50, 8, 0.5); err == nil {
		t.Fatal("start beyond buffer accepted")
	}
}
