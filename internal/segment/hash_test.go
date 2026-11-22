package segment

import (
	"strings"
	"testing"
)

func TestSHA256Deterministic(t *testing.T) {
	r := Record{
		Index: 1, StartNs: 0, EndNs: 1_000_000_000, Recovery: Clean,
		TrackIDs: []uint32{2, 1}, IssueClasses: []string{"b", "a"},
		Sources: []Source{{Kind: "media", Ref: "v", ByteOffset: 10}, {Kind: "subtitle", Ref: "s", CueIndex: 3}},
	}
	h1 := SHA256(r)
	h2 := SHA256(r)
	if h1 != h2 {
		t.Fatalf("hash unstable: %s vs %s", h1, h2)
	}
	// Reordering the slices must not change the digest.
	r2 := r
	r2.TrackIDs = []uint32{1, 2}
	r2.IssueClasses = []string{"a", "b"}
	r2.Sources = []Source{{Kind: "subtitle", Ref: "s", CueIndex: 3}, {Kind: "media", Ref: "v", ByteOffset: 10}}
	if SHA256(r2) != h1 {
		t.Fatal("hash depends on slice order")
	}
}

func TestSHA256SensitiveToFacts(t *testing.T) {
	base := Record{Index: 1, StartNs: 0, EndNs: 1_000_000_000, Recovery: Clean}
	mods := []func(*Record){
		func(r *Record) { r.EndNs++ },
		func(r *Record) { r.Recovery = Recovered },
		func(r *Record) { r.TrackIDs = []uint32{1} },
		func(r *Record) { r.IssueClasses = []string{"x"} },
		func(r *Record) { r.Sources = []Source{{Kind: "k"}} },
	}
	baseHash := SHA256(base)
	seen := map[string]bool{baseHash: true}
	for i, m := range mods {
		r := base
		m(&r)
		h := SHA256(r)
		if seen[h] {
			t.Errorf("modification %d produced a colliding hash", i)
		}
		seen[h] = true
	}
}

func TestCanonicalIsLengthPrefixed(t *testing.T) {
	// Concatenation ambiguity must be impossible: ("ab","c") and ("a","bc")
	// encode differently because each field is length-prefixed.
	a := Canonical(Record{Index: 1, IssueClasses: []string{"ab", "c"}})
	b := Canonical(Record{Index: 1, IssueClasses: []string{"a", "bc"}})
	if string(a) == string(b) {
		t.Fatal("ambiguous canonical encoding")
	}
	if !strings.Contains(string(a), "2:ab|") {
		t.Errorf("expected length prefix in %q", a)
	}
}

func TestShort(t *testing.T) {
	h := strings.Repeat("ab", 32)
	if got := Short(h, 12); got != strings.Repeat("ab", 6) {
		t.Errorf("Short = %q", got)
	}
	if got := Short("abc", 12); got != "abc" {
		t.Errorf("Short shorter than n = %q", got)
	}
}
