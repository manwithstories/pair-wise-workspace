package audio

import (
	"testing"

	"avpipe/internal/media"
)

func TestResampleRatio(t *testing.T) {
	r, err := NewResampler(48_000, 16_000)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]int16, 4800) // 100 ms mono
	for i := range in {
		in[i] = int16(i % 200)
	}
	out := r.Process(in, 1)
	if len(out) != 1600 {
		t.Fatalf("out samples=%d, want 1600", len(out))
	}
}

func TestResampleLongStreamErrorWithinOne(t *testing.T) {
	r, _ := NewResampler(48_000, 16_000)
	// Feed 48 kHz in 10 ms mono blocks for 1000 blocks (10 s).
	var produced uint64
	for b := 0; b < 1000; b++ {
		in := make([]int16, 480)
		for i := range in {
			in[i] = int16((b*480 + i) % 1000)
		}
		out := r.Process(in, 1)
		produced += uint64(len(out))
	}
	want := ExpectedOutputFrames(480_000, 48_000, 16_000)
	diff := int64(produced) - int64(want)
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("produced=%d want=%d error=%d exceeds 1 sample", produced, want, diff)
	}
}

func TestResampleStereoInterleave(t *testing.T) {
	r, _ := NewResampler(48_000, 16_000)
	in := make([]int16, 960) // 480 stereo frames
	out := r.Process(in, 2)
	if len(out) != 320 {
		t.Fatalf("stereo out samples=%d, want 320", len(out))
	}
}

func TestResampleIdentity(t *testing.T) {
	r, _ := NewResampler(16_000, 16_000)
	in := []int16{1, 2, 3, 4}
	out := r.Process(in, 1)
	if len(out) != 4 || out[2] != 3 {
		t.Fatalf("identity resample broken: %v", out)
	}
}

func TestResampleRejectsZeroRate(t *testing.T) {
	_, err := NewResampler(0, 16_000)
	if err == nil {
		t.Fatal("expected error for zero input rate")
	}
	if me, ok := err.(*media.Error); !ok || me.Action != media.ActionFail {
		t.Fatalf("want locatable fail error, got %v", err)
	}
}
