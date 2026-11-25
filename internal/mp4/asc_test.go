package mp4

import "testing"

func TestFindASC(t *testing.T) {
	// Spec-correct nested descriptors:
	// ES(03) -> DecoderConfig(04) -> DecoderSpecificInfo(05: 12 30), SL(06)
	esds := []byte{
		0x03, 0x19, 0x00, 0x02, 0x00,
		0x04, 0x14,
		0x40, 0x15, 0x00, 0x00, 0x00, 0x00, 0x05, 0x34,
		0x08, 0x00, 0x00, 0x00, 0x00,
		0x05, 0x02, 0x12, 0x30,
		0x06, 0x01, 0x02,
	}
	asc, ok := findASC(esds)
	if !ok || len(asc) != 2 || asc[0] != 0x12 || asc[1] != 0x30 {
		t.Fatalf("want ASC 1230, got %x (%v)", asc, ok)
	}
	ch, rate, pok := parseASC(asc)
	if ch != 6 || rate != 44100 || !pok {
		t.Fatalf("want 6ch/44100, got %d/%d/%v", ch, rate, pok)
	}
}

func TestParseASCVariants(t *testing.T) {
	cases := []struct {
		asc      []byte
		ch, rate uint32
	}{
		{[]byte{0x11, 0x90}, 2, 48000}, // LC 48k stereo
		{[]byte{0x12, 0x10}, 2, 44100}, // LC 44.1k stereo
		{[]byte{0x12, 0x88}, 1, 32000}, // LC 32k mono
		{[]byte{0x12, 0x30}, 6, 44100}, // LC 44.1k 5.1 (real MOV fixture)
	}
	for i, c := range cases {
		ch, rate, ok := parseASC(c.asc)
		if !ok || uint32(ch) != c.ch || rate != c.rate {
			t.Fatalf("case %d: got ch=%d rate=%d ok=%v", i, ch, rate, ok)
		}
	}
}
