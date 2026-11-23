package fixture

import (
	"bytes"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func TestBuildBothVariants(t *testing.T) {
	for variant := 0; variant < 2; variant++ {
		art, err := Build(DefaultSpec(42, variant))
		if err != nil {
			t.Fatalf("variant %d: %v", variant, err)
		}
		if len(art.Tracks) != 3 {
			t.Fatalf("variant %d: want 3 tracks, got %d", variant, len(art.Tracks))
		}
		// go-mp4 must accept the structure as valid ISO BMFF.
		r := bytes.NewReader(art.Bytes)
		boxes, err := mp4.ExtractBoxes(r, nil, []mp4.BoxPath{
			{mp4.BoxTypeFtyp()},
			{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()},
			{mp4.BoxTypeMdat()},
		})
		if err != nil {
			t.Fatalf("variant %d: go-mp4 rejected output: %v", variant, err)
		}
		var traks, mdats int
		for _, b := range boxes {
			switch b.Type {
			case mp4.BoxTypeTrak():
				traks++
			case mp4.BoxTypeMdat():
				mdats++
			}
		}
		if traks != 3 || mdats != 1 {
			t.Fatalf("variant %d: traks=%d mdats=%d", variant, traks, mdats)
		}
	}
}

func TestDamageModes(t *testing.T) {
	cases := []Mode{ModeTruncatedTail, ModeCorruptSample, ModeMissingIndex,
		ModeIndexOutOfRange, ModeTruncatedHeader}
	for _, mode := range cases {
		spec := DefaultSpec(7, 0)
		spec.Mode = mode
		art, err := Build(spec)
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		if len(art.Bytes) == 0 {
			t.Fatalf("mode %s produced empty output", mode)
		}
		switch mode {
		case ModeCorruptSample:
			if art.CorruptOffset < 0 {
				t.Fatalf("mode %s missing corrupt offset", mode)
			}
		case ModeTruncatedTail, ModeTruncatedHeader:
			if art.OriginalLength <= int64(len(art.Bytes)) {
				t.Fatalf("mode %s did not shorten file", mode)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := Build(DefaultSpec(42, 0))
	b, _ := Build(DefaultSpec(42, 0))
	if !bytes.Equal(a.Bytes, b.Bytes) {
		t.Fatal("same seed must produce identical bytes")
	}
}
