package codec_test

import (
	"testing"

	"mediapack/internal/codec"
)

func TestCodecStringRoundTrip(t *testing.T) {
	for _, c := range []codec.Codec{codec.H264, codec.H265, codec.AV1} {
		if c.String() == "unknown" {
			t.Fatalf("codec %v has no string form", c)
		}
	}
	if code := codec.Codec(99); code.String() != "unknown" {
		t.Fatalf("unknown codec must render as 'unknown', got %q", code.String())
	}
}

func TestAccessUnitTimebase(t *testing.T) {
	au := codec.AccessUnit{}
	if au.Timebase().Num != 90000 || au.Timebase().Den != 1 {
		t.Fatalf("unified timebase wrong: %+v", au.Timebase())
	}
}
