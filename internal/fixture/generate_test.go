package fixture

import (
	"bytes"
	"testing"
)

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(Options{Seed: 42, Fragments: 6, DriftPPM: 120, WrapAt: 1 << 33, VFR: true})
	b := Generate(Options{Seed: 42, Fragments: 6, DriftPPM: 120, WrapAt: 1 << 33, VFR: true})
	if !bytes.Equal(a.Init, b.Init) || len(a.Fragments) != len(b.Fragments) {
		t.Fatal("same seed produced different streams")
	}
	for i := range a.Fragments {
		if !bytes.Equal(a.Fragments[i], b.Fragments[i]) {
			t.Fatalf("fragment %d differs across runs", i)
		}
	}
	c := Generate(Options{Seed: 43, Fragments: 6, VFR: true})
	if bytes.Equal(a.Fragments[0], c.Fragments[0]) {
		t.Fatal("different seeds produced identical fragments")
	}
}

func TestGenerateFragmentCount(t *testing.T) {
	s := Generate(Options{Seed: 1, Fragments: 5})
	if len(s.Fragments) != 10 { // 5 video + 5 audio interleaved
		t.Fatalf("fragments=%d", len(s.Fragments))
	}
	if len(s.Capture) != len(s.Fragments) {
		t.Fatalf("capture=%d", len(s.Capture))
	}
	// Video fragments carry capture packets; audio fragments do not.
	if s.Capture[0] == nil || s.Capture[1] != nil {
		t.Fatal("capture alignment wrong")
	}
}

func TestCorruptModesChangeStream(t *testing.T) {
	base := Generate(Options{Seed: 1, Fragments: 8})
	for _, m := range []CorruptMode{
		CorruptTruncatedBox, CorruptSampleRange, CorruptSeqGap,
		CorruptTFDTRegression, CorruptDuplicate,
	} {
		s := Generate(Options{Seed: 1, Fragments: 8, Corrupt: m})
		if m == CorruptSeqGap {
			if len(s.Fragments) != len(base.Fragments)-2 {
				t.Fatalf("%s: fragments=%d", m, len(s.Fragments))
			}
			continue
		}
		if len(s.Fragments) != len(base.Fragments) {
			t.Fatalf("%s: fragments=%d", m, len(s.Fragments))
		}
		same := true
		for i := range s.Fragments {
			if !bytes.Equal(s.Fragments[i], base.Fragments[i]) {
				same = false
			}
		}
		if same {
			t.Fatalf("%s did not alter the stream", m)
		}
	}
}

func TestParseCorruptMode(t *testing.T) {
	if _, err := ParseCorruptMode("truncated-box"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCorruptMode("nope"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}
