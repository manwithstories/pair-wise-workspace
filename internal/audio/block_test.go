package audio

import (
	"testing"

	"avpipe/internal/fault"
)

func TestDecodeRejectsNonPositiveBlockSize(t *testing.T) {
	f := testFormat(48000, 1, 16, 0)
	img, _ := Encode(f, make([]float64, 10), 10)
	info, _ := Parse(img, "mono")
	for _, n := range []int{0, -5} {
		_, err := Decode(img, info, n)
		if e := fault.Root(err); e == nil || e.Code != fault.CodeInvalidArgument {
			t.Fatalf("block size %d accepted: %v", n, err)
		}
	}
}

func TestDecodeBlockLargerThanStreamYieldsOneBlock(t *testing.T) {
	f := testFormat(48000, 2, 16, 0)
	img, _ := Encode(f, make([]float64, 40), 20)
	info, _ := Parse(img, "stereo")
	blocks, err := Decode(img, info, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Frames != 20 {
		t.Fatalf("blocks=%d frames=%d", len(blocks), blocks[0].Frames)
	}
}

func TestDecodeExactMultipleHasNoShortTail(t *testing.T) {
	f := testFormat(48000, 1, 16, 0)
	img, _ := Encode(f, make([]float64, 1000), 1000)
	info, _ := Parse(img, "mono")
	blocks, _ := Decode(img, info, 250)
	if len(blocks) != 4 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	for i, b := range blocks {
		if b.Frames != 250 || int64(b.StartFrame) != int64(i*250) {
			t.Fatalf("block %d frames=%d start=%d", i, b.Frames, b.StartFrame)
		}
	}
}
