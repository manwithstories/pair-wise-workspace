package segmenthash

import (
	"bytes"
	"strings"
	"testing"
)

func TestDigestDeterminism(t *testing.T) {
	a, err := Digest(strings.NewReader("hello segment"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Digest(strings.NewReader("hello segment"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("digest not deterministic: %s vs %s", a, b)
	}
	if a != Bytes([]byte("hello segment")) {
		t.Fatal("Digest and Bytes disagree")
	}
	if len(a) != 64 {
		t.Fatalf("digest length = %d", len(a))
	}
	if a == Bytes([]byte("other")) {
		t.Fatal("collision")
	}
}

func TestWriterMatchesDigest(t *testing.T) {
	w := NewWriter()
	w.WriteString("hello ")
	w.Write([]byte("segment"))
	if w.Sum() != Bytes([]byte("hello segment")) {
		t.Fatalf("incremental sum = %s", w.Sum())
	}
}

func TestRecordValidation(t *testing.T) {
	ok := Record{URI: "a.m4s", Role: "input", SHA256: Bytes([]byte("x"))}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Record{
		{URI: "", Role: "input", SHA256: ok.SHA256},
		{URI: "a", Role: "other", SHA256: ok.SHA256},
		{URI: "a", Role: "input", SHA256: "abc"},
		{URI: "a", Role: "input", SHA256: ok.SHA256, Size: -1},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
	}
}

func TestCompare(t *testing.T) {
	x := Bytes([]byte("x"))
	y := Bytes([]byte("y"))
	ref := []Record{
		{URI: "a.m4s", Role: "input", Size: 1, SHA256: x},
		{URI: "b.m4s", Role: "input", Size: 1, SHA256: y},
	}
	out := []Record{
		{URI: "a.m4s", Role: "output", Size: 1, SHA256: x},
		{URI: "b.m4s", Role: "output", Size: 1, SHA256: x},
		{URI: "c.m4s", Role: "output", Size: 1, SHA256: x},
	}
	c := Compare(ref, out)
	if c.Matched != 1 || c.Total != 2 {
		t.Fatalf("comparison = %+v", c)
	}
	if c.Ratio() != 50 {
		t.Fatalf("ratio = %v", c.Ratio())
	}
	if len(c.Mismatch) != 2 || c.Mismatch[0] != "b.m4s" {
		t.Fatalf("mismatch = %v", c.Mismatch)
	}
	empty := Compare(nil, nil)
	if empty.Ratio() != 0 {
		t.Fatal("empty ratio must be 0")
	}
}

func TestManifestOrdering(t *testing.T) {
	recs := []Record{
		{URI: "b", Role: "input"},
		{URI: "a", Role: "output"},
		{URI: "a", Role: "input"},
	}
	got := Manifest(recs)
	if got[0].URI != "a" || got[0].Role != "input" || got[1].Role != "output" || got[2].URI != "b" {
		t.Fatalf("order = %+v", got)
	}
	if !bytes.Equal([]byte(got[0].URI), []byte("a")) {
		t.Fatal("unexpected corruption")
	}
}
