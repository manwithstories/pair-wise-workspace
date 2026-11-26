package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	st := &State{ManifestSHA: "m", SourcesSHA: "s", ParamsSHA: "p", Completed: 7, FrameSHAs: []string{"a", "b"}}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestSHA != st.ManifestSHA || got.SourcesSHA != st.SourcesSHA ||
		got.ParamsSHA != st.ParamsSHA || got.Completed != st.Completed ||
		len(got.FrameSHAs) != 2 || got.FrameSHAs[1] != "b" {
		t.Fatalf("roundtrip = %+v, want %+v", got, st)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestLoadMissing(t *testing.T) {
	st, err := NewStore(t.TempDir()).Load()
	if err != nil || st != nil {
		t.Fatalf("missing checkpoint = %v, %v", st, err)
	}
}

func TestValidate(t *testing.T) {
	st := &State{ManifestSHA: "m", SourcesSHA: "s", ParamsSHA: "p"}
	if err := Validate(st, "m", "s", "p"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]string{{"x", "s", "p"}, {"m", "x", "p"}, {"m", "s", "x"}} {
		if err := Validate(st, bad[0], bad[1], bad[2]); err == nil {
			t.Fatalf("mismatch %v accepted", bad)
		}
	}
}

func TestDigestDeterministic(t *testing.T) {
	a := Digest("alpha", "beta")
	b := Digest("alpha", "beta")
	c := Digest("alpha", "gamma")
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("digest broken: %s %s %s", a, b, c)
	}
	// 长度前缀防止拼接歧义
	if Digest("ab", "c") == Digest("a", "bc") {
		t.Fatal("digest ambiguous")
	}
}
