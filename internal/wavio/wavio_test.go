package wavio

import (
	"bytes"
	"io"
	"math"
	"testing"
)

func roundTrip(t *testing.T, hdr WriteHeader, planar [][]float64) {
	t.Helper()
	file := &memSink{}
	writer, err := NewWriter(file, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteFrames(planar); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(bytes.NewReader(file.buf))
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().SampleRate != hdr.SampleRate || reader.Header().Channels != hdr.Channels {
		t.Fatalf("header mismatch: %+v", reader.Header())
	}
	tolerance := 1.0 / 32768
	if hdr.BitsPerSample == 24 {
		tolerance = 1.0 / 8388608
	}
	if hdr.IsFloat && hdr.BitsPerSample == 32 {
		tolerance = 1e-6
	}
	checked := 0
	for {
		got, err := reader.ReadFrames(1024)
		if len(got) > 0 {
			for ch := range planar {
				for i, sample := range got[ch] {
					index := checked + i
					if math.Abs(sample-planar[ch][index]) > tolerance {
						t.Fatalf("ch %d sample %d: got %.6f want %.6f", ch, index, sample, planar[ch][index])
					}
				}
			}
			checked += len(got[0])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked != len(planar[0]) {
		t.Fatalf("read %d frames, want %d", checked, len(planar[0]))
	}
}

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

func TestRoundTripAllCodecs(t *testing.T) {
	planar := TonePlanar(48000, 1, 2, 1000, 0.5, 0.3)
	cases := []WriteHeader{
		{SampleRate: 48000, Channels: 2, BitsPerSample: 16},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 24},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 32},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 32, IsFloat: true},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 64, IsFloat: true},
	}
	for _, hdr := range cases {
		roundTrip(t, hdr, planar)
	}
}

func TestTruncatedReportsOffset(t *testing.T) {
	planar := TonePlanar(48000, 2, 1, 1000, 0.4, 0)
	hdr := WriteHeader{SampleRate: 48000, Channels: 1, BitsPerSample: 24}
	if err := WriteTruncated(t.TempDir()+"/trunc.wav", hdr, planar, 100); err != nil {
		t.Fatal(err)
	}
}
