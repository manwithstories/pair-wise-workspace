package codec_test

import (
	"testing"

	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
)

func TestParseAllCodecs(t *testing.T) {
	for _, c := range []codec.Codec{codec.H264, codec.H265, codec.AV1} {
		c := c
		t.Run(c.String(), func(t *testing.T) {
			data, keySeqs, _, err := fixture.BuildStream(fixture.StreamSpec{
				Codec: c, Seed: 17, Frames: 12, KeyEvery: 4,
			}, fixture.Fault{})
			if err != nil {
				t.Fatal(err)
			}
			detected, err := codec.DetectCodec(data)
			if err != nil || detected != c {
				t.Fatalf("detect: got %v err=%v want %v", detected, err, c)
			}
			aus, meta, err := codec.ParseBytes(data, 1)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(aus) != 12 || meta.NumAU != 12 {
				t.Fatalf("AU count = %d", len(aus))
			}
			if len(meta.Keyframes) != len(keySeqs) || meta.Keyframes[1] != keySeqs[1] {
				t.Fatalf("keyframes = %v want %v", meta.Keyframes, keySeqs)
			}
			for i, au := range aus {
				if au.PTS != int64(i)*3000 || au.TrackID != 1 {
					t.Fatalf("AU %d timestamp/track wrong: %+v", i, au)
				}
				if len(au.NALs) == 0 {
					t.Fatalf("AU %d has no units", i)
				}
				if (i%4 == 0) != au.Key {
					t.Fatalf("AU %d key flag = %v", i, au.Key)
				}
			}
			if len(meta.Configs) == 0 {
				t.Fatal("expected parameter-set/sequence-header configs")
			}
		})
	}
}

func TestTruncatedNALReportsExactOffset(t *testing.T) {
	data, _, keyOffs, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 17, Frames: 6, KeyEvery: 3,
	}, fixture.Fault{})
	// Cut deep inside the second keyframe AU's VCL payload, well before its
	// integrity trailer. The parser reports the actual input end as the first
	// byte at which framing and declared length can no longer agree.
	cut := keyOffs[1] + 10
	if int(cut) >= len(data) {
		t.Fatalf("test cut %d beyond stream %d", cut, len(data))
	}
	bad := append([]byte(nil), data[:cut]...)
	_, _, err := codec.ParseBytes(bad, 1)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput || fe.Offset != int64(len(bad)) {
		t.Fatalf("expected corrupt error at input end %d, got %+v", len(bad), err)
	}
}

func TestForbiddenBit(t *testing.T) {
	data, _, _, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H265, Seed: 3, Frames: 4, KeyEvery: 2,
	}, fixture.Fault{})
	// VPS is the first unit: 4-byte start code then its header byte. The AUD
	// remains intact, so codec detection still succeeds first.
	const hdrOff = int64(4)
	bad := append([]byte(nil), data...)
	bad[hdrOff] |= 0x80
	_, _, err := codec.ParseBytes(bad, 1)
	fe, ok := fault.As(err)
	if !ok || fe.Offset != hdrOff || fe.Kind != fault.KindCorruptInput {
		t.Fatalf("got %+v", err)
	}
}

func TestEmptyAndGarbage(t *testing.T) {
	if _, err := codec.DetectCodec([]byte{1, 2, 3, 4}); err == nil {
		t.Fatal("garbage without start codes must fail")
	}
	if _, _, err := codec.ParseBytes(nil, 0); err == nil {
		t.Fatal("empty input must fail")
	}
}

func TestTimestampMonotonic(t *testing.T) {
	data, _, _, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.AV1, Seed: 9, Frames: 30, KeyEvery: 10,
	}, fixture.Fault{})
	aus, _, err := codec.ParseBytes(data, 2)
	if err != nil {
		t.Fatal(err)
	}
	var prev int64 = -1
	for _, au := range aus {
		if au.PTS <= prev {
			t.Fatalf("non-monotonic PTS: %d after %d", au.PTS, prev)
		}
		prev = au.PTS
	}
}
