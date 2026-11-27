package fixture

import (
	"crypto/sha256"
	"testing"

	"bcastqc/internal/ebml"
)

func TestGenerateDeterministic(t *testing.T) {
	s := SpecFor(SuiteRecovery, 42, 4<<20)
	h := func() [32]byte {
		out, err := Generate(s)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(out)
	}
	if h() != h() {
		t.Fatal("fixture output nondeterministic")
	}
}

func TestGeneratedFileParses(t *testing.T) {
	for _, suite := range AllSuites {
		s := SpecFor(suite, 42, 4<<20)
		out, err := Generate(s)
		if err != nil {
			t.Fatalf("%s: %v", suite, err)
		}
		doc, err := ebml.ParseDocument(ebml.NewBytesReader(out))
		if s.ExpectReject {
			if err == nil {
				t.Fatalf("%s: truncated fixture must fail parsing", suite)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: parse failed: %v", suite, err)
		}
		if len(doc.Tracks) != 2 || len(doc.Blocks) == 0 {
			t.Fatalf("%s: unexpected tracks=%d blocks=%d",
				suite, len(doc.Tracks), len(doc.Blocks))
		}
	}
}

func TestInterleavedMonotonicTimeline(t *testing.T) {
	s := SpecFor(SuiteClean, 1, 4<<20)
	out, _ := Generate(s)
	doc, err := ebml.ParseDocument(ebml.NewBytesReader(out))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(doc.Blocks); i++ {
		if doc.Blocks[i].TimeNS < doc.Blocks[i-1].TimeNS {
			t.Fatalf("timeline not monotonic at %d", i)
		}
	}
}
