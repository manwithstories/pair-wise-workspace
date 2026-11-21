package audio

import (
	"bytes"
	"testing"

	"mediad/internal/model"
)

func pcm(samples ...int16) []byte {
	var b bytes.Buffer
	for _, s := range samples {
		lo := byte(s)
		hi := byte(s >> 8)
		b.WriteByte(lo)
		b.WriteByte(hi)
	}
	return b.Bytes()
}

func TestDetect_BasicIntervalsAndTimelineOffset(t *testing.T) {
	d, err := NewDetector(Config{SampleRate: 1000, Threshold: 10, MinSilenceMS: 200})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 samples = 1000 ms at 1 kHz: loud 100, silent 400, loud 200, silent 300
	in := make([]int16, 0, 1000)
	for range 100 {
		in = append(in, 500)
	}
	for range 400 {
		in = append(in, 0)
	}
	for range 200 {
		in = append(in, -900)
	}
	for range 300 {
		in = append(in, 5)
	}
	offset := model.MustRat(2, 1) // segment starts at 2000 ms
	ivs, err := d.Detect(bytes.NewReader(pcm(in...)), offset)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(ivs) != 2 {
		t.Fatalf("intervals = %+v, want 2", ivs)
	}
	if ivs[0].StartMS != 2100 || ivs[0].EndMS != 2500 {
		t.Fatalf("first interval = %+v, want [2100,2500)", ivs[0])
	}
	if ivs[1].StartMS != 2700 || ivs[1].EndMS != 3000 {
		t.Fatalf("second interval = %+v, want [2700,3000)", ivs[1])
	}
}

func TestDetect_ShortRunsAreDropped(t *testing.T) {
	d, _ := NewDetector(Config{SampleRate: 1000, Threshold: 0, MinSilenceMS: 500})
	// 100 ms silence surrounded by audio: below threshold, must not appear.
	in := make([]int16, 0, 500)
	for range 200 {
		in = append(in, 100)
	}
	for range 100 {
		in = append(in, 0)
	}
	for range 200 {
		in = append(in, 100)
	}
	ivs, err := d.Detect(bytes.NewReader(pcm(in...)), model.MustRat(0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 0 {
		t.Fatalf("sub-threshold run must be dropped, got %+v", ivs)
	}
}

func TestDetect_ThresholdBoundaryAndTrailingRun(t *testing.T) {
	d, _ := NewDetector(Config{SampleRate: 1000, Threshold: 3, MinSilenceMS: 0})
	in := []int16{3, 2, -3, 100, 0, 0}
	ivs, err := d.Detect(bytes.NewReader(pcm(in...)), model.MustRat(0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 2 {
		t.Fatalf("want two runs, got %+v", ivs)
	}
	if ivs[0].StartMS != 0 || ivs[0].EndMS != 3 {
		t.Fatalf("boundary run = %+v, want [0,3)", ivs[0])
	}
	if ivs[1].StartMS != 4 || ivs[1].EndMS != 6 {
		t.Fatalf("trailing run = %+v, want [4,6)", ivs[1])
	}
}

func TestDetect_BadConfigAndTruncatedTail(t *testing.T) {
	if _, err := NewDetector(Config{SampleRate: 0}); err == nil {
		t.Fatal("zero sample rate must fail")
	}
	if _, err := NewDetector(Config{SampleRate: 1000, MinSilenceMS: -1}); err == nil {
		t.Fatal("negative min silence must fail")
	}
	d, _ := NewDetector(Config{SampleRate: 1000, Threshold: 0, MinSilenceMS: 0})
	// One complete sample plus one dangling byte: ignore the partial sample.
	data := append(pcm(0), 0xAA)
	ivs, err := d.Detect(bytes.NewReader(data), model.MustRat(0, 1))
	if err != nil {
		t.Fatalf("dangling byte must be tolerated: %v", err)
	}
	if len(ivs) != 1 || ivs[0].EndMS != 1 {
		t.Fatalf("partial tail must not extend interval, got %+v", ivs)
	}
}
