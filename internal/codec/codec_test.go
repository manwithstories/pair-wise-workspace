package codec

import (
	"encoding/binary"
	"errors"
	"testing"
)

func opusPayload(config, frames byte, rest ...byte) []byte {
	toc := config<<3 | frames<<1
	return append([]byte{toc}, rest...)
}

func TestOpusFrameSizes(t *testing.T) {
	cases := []struct {
		config byte
		wantUS int64
	}{
		{0, 10000}, {1, 20000}, {2, 40000}, {3, 60000}, // SILK NB
		{12, 10000}, {13, 20000}, // Hybrid
		{16, 2500}, {17, 5000}, {18, 10000}, {19, 20000}, // CELT NB
		{28, 2500}, {29, 5000}, {30, 10000}, {31, 20000}, // CELT FB
	}
	for _, c := range cases {
		got, err := OpusDurationUS(opusPayload(c.config, 0, 0xAA, 0xBB))
		if err != nil || got != c.wantUS {
			t.Fatalf("config %d: got %d/%v want %d", c.config, got, err, c.wantUS)
		}
	}
}

func TestOpusTwoEqualFrames(t *testing.T) {
	got, err := OpusDurationUS(opusPayload(22, 1, 0x01))
	if err != nil || got != 20000 {
		t.Fatalf("got %d/%v", got, err)
	}
}

func TestOpusTwoLengthCodedFrames(t *testing.T) {
	// config 21 = 5 ms; two length-coded frames of equal duration.
	got, err := OpusDurationUS(opusPayload(21, 2, 0x03, 1, 2, 3, 4))
	if err != nil || got != 10000 {
		t.Fatalf("got %d/%v", got, err)
	}
}

func TestOpusArbitraryFrames(t *testing.T) {
	// config 22 = 10 ms, code 3 with 3 frames and 6 payload bytes.
	got, err := OpusDurationUS(opusPayload(22, 3, 3, 1, 2, 3, 4, 5, 6))
	if err != nil || got != 30000 {
		t.Fatalf("got %d/%v", got, err)
	}
	// Inconsistent length table is rejected, not guessed.
	if _, err := OpusDurationUS(opusPayload(22, 3, 3, 1, 2, 3, 4, 5)); !errors.Is(err, ErrUnderivable) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpusTruncated(t *testing.T) {
	if _, err := OpusDurationUS(nil); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("nil err = %v", err)
	}
	if _, err := OpusDurationUS([]byte{0x00}); err != nil {
		// config 0 code 0 accepts an empty body; that is a valid short frame.
		t.Fatalf("single TOC rejected: %v", err)
	}
	if _, err := OpusDurationUS(opusPayload(21, 2)); !errors.Is(err, ErrUnderivable) {
		t.Fatalf("missing length err = %v", err)
	}
}

func aacPayload(aus ...[]byte) []byte {
	cfg := DefaultAACConfig()
	stride := cfg.SizeLength + cfg.IndexLength
	totalBits := stride * len(aus)
	v := uint64(0)
	pos := 0
	for _, au := range aus {
		v = v<<uint(cfg.SizeLength) | uint64(len(au))
		pos += cfg.SizeLength
		// AU-index left zero.
		v <<= uint(cfg.IndexLength)
		pos += cfg.IndexLength
	}
	nbytes := (totalBits + 7) / 8
	header := make([]byte, nbytes)
	for i := 0; i < pos; i++ {
		if v>>uint(pos-1-i)&1 == 1 {
			header[i/8] |= 1 << uint(7-i%8)
		}
	}
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, uint16(totalBits))
	out = append(out, header...)
	for _, au := range aus {
		out = append(out, au...)
	}
	return out
}

func TestAACDuration(t *testing.T) {
	p := aacPayload(make([]byte, 100), make([]byte, 80))
	got, err := DefaultAACConfig().AACDurationUS(p)
	if err != nil {
		t.Fatal(err)
	}
	// 2 AUs * 1024 samples / 48000 = 42666.67 us truncated.
	if want := int64(42666); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestAACRejectsInconsistent(t *testing.T) {
	p := aacPayload(make([]byte, 100))
	p = p[:len(p)-1] // declared AU size no longer matches payload
	if _, err := DefaultAACConfig().AACDurationUS(p); !errors.Is(err, ErrUnderivable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := DefaultAACConfig().AACDurationUS([]byte{0, 0}); !errors.Is(err, ErrUnderivable) {
		t.Fatalf("zero header err = %v", err)
	}
}

func TestAACConfigValidate(t *testing.T) {
	bad := DefaultAACConfig()
	bad.SizeLength = 0
	if bad.Validate() == nil {
		t.Fatal("zero size length accepted")
	}
	bad = DefaultAACConfig()
	bad.SamplesPerAU = -1
	if bad.Validate() == nil {
		t.Fatal("negative samples accepted")
	}
}
