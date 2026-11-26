package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func pcm(frames ...[]int16) []byte {
	var buf bytes.Buffer
	for _, f := range frames {
		for _, s := range f {
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], uint16(s))
			buf.Write(b[:])
		}
	}
	return buf.Bytes()
}

func run(t *testing.T, data []byte, spec Spec) ([]byte, int64) {
	t.Helper()
	var out bytes.Buffer
	n, err := Normalize(bytes.NewReader(data), &out, spec)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(out.Bytes())) != n*4 {
		t.Fatalf("output bytes %d != frames %d*4", len(out.Bytes()), n)
	}
	return out.Bytes(), n
}

func TestMonoPassthrough48k(t *testing.T) {
	data := pcm([]int16{100}, []int16{-200}, []int16{300})
	out, n := run(t, data, Spec{LayoutMono, 48000})
	if n != 3 {
		t.Fatalf("frames=%d", n)
	}
	for i, want := range []int16{100, -200, 300} {
		l := int16(binary.LittleEndian.Uint16(out[i*4:]))
		r := int16(binary.LittleEndian.Uint16(out[i*4+2:]))
		if l != want || r != want {
			t.Fatalf("frame %d: got (%d,%d) want %d", i, l, r, want)
		}
	}
}

func TestStereo96kDecimate(t *testing.T) {
	var frames [][]int16
	for i := 0; i < 960; i++ {
		frames = append(frames, []int16{int16(i), int16(-i)})
	}
	_, n := run(t, pcm(frames...), Spec{LayoutStereo, 96000})
	if n != 480 {
		t.Fatalf("frames=%d want 480", n)
	}
}

func TestStereo44100Upsample(t *testing.T) {
	frames := make([][]int16, 44100)
	for i := range frames {
		frames[i] = []int16{int16(i % 1000), int16(i % 1000)}
	}
	_, n := run(t, pcm(frames...), Spec{LayoutStereo, 44100})
	if n != 48000 {
		t.Fatalf("frames=%d want 48000", n)
	}
}

func TestFiveOneDownmix(t *testing.T) {
	// L=1024, R=-1024, C=1024, LFE=max, Ls=0, Rs=0
	data := pcm([]int16{1024, -1024, 1024, 32767, 0, 0})
	out, n := run(t, data, Spec{LayoutFiveOne, 48000})
	if n != 1 {
		t.Fatalf("frames=%d", n)
	}
	l := int16(binary.LittleEndian.Uint16(out))
	r := int16(binary.LittleEndian.Uint16(out[2:]))
	wantL := int16((1024*1024 + 1024*724) >> 10)
	wantR := int16((-1024*1024 + 1024*724) >> 10)
	if l != wantL || r != wantR {
		t.Fatalf("got (%d,%d) want (%d,%d)", l, r, wantL, wantR)
	}
}

func TestUnsupported(t *testing.T) {
	if _, ok := ParseLayout("7.1"); ok {
		t.Fatal("7.1 must be unsupported")
	}
	var out bytes.Buffer
	if _, err := Normalize(bytes.NewReader(pcm([]int16{1})), &out,
		Spec{LayoutUnknown, 48000}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("layout: %v", err)
	}
	if _, err := Normalize(bytes.NewReader(pcm([]int16{1, 2})), &out,
		Spec{LayoutStereo, 22050}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("rate: %v", err)
	}
}
