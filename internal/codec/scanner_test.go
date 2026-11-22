package codec

import (
	"bytes"
	"io"
	"testing"
)

func TestAnnexBScannerWindowBoundaries(t *testing.T) {
	// Units sized so several read-window refills happen; mixes 3 and 4 byte
	// start codes and trailing zero padding.
	var in []byte
	wantUnits := 40
	for i := 0; i < wantUnits; i++ {
		if i%3 == 0 {
			in = append(in, 0, 0, 0, 1)
		} else {
			in = append(in, 0, 0, 1)
		}
		body := bytes.Repeat([]byte{byte('A' + i%26)}, 5000+(i%7)*997)
		in = append(in, body...)
		in = append(in, 0, 0) // padding zeros before next code
	}
	sc := newAnnexBScanner(bytes.NewReader(in), MaxNALBytes)
	got := 0
	var lastOff int64
	for {
		raw, off, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unit %d: %v", got, err)
		}
		if off <= lastOff && got > 0 {
			t.Fatalf("offsets not monotonic: %d after %d", off, lastOff)
		}
		if len(raw) < 5000 {
			t.Fatalf("unit %d unexpectedly short: %d", got, len(raw))
		}
		lastOff, got = off, got+1
	}
	if got != wantUnits {
		t.Fatalf("units = %d want %d", got, wantUnits)
	}
}

func TestAnnexBScannerLeadingGarbage(t *testing.T) {
	in := append([]byte{0x77, 0x88}, []byte{0, 0, 1, 9, 0, 0, 1, 1}...)
	sc := newAnnexBScanner(bytes.NewReader(in), MaxNALBytes)
	raw, _, err := sc.next()
	if err != nil || len(raw) != 1 || raw[0] != 9 {
		t.Fatalf("first unit wrong: %v err=%v", raw, err)
	}
	if _, _, err = sc.next(); err != nil {
		t.Fatalf("second unit: %v", err)
	}
	if _, _, err = sc.next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestAnnexBScannerOversizedUnit(t *testing.T) {
	in := append([]byte{0, 0, 1}, bytes.Repeat([]byte{0x55}, 200)...)
	sc := newAnnexBScanner(bytes.NewReader(in), 50)
	if _, _, err := sc.next(); err == nil {
		t.Fatal("expected size-limit error")
	}
}
