package index

import (
	"os"
	"path/filepath"
	"testing"

	"aircheck/internal/fault"
)

type payload struct {
	Tracks int    `json:"tracks"`
	Note   string `json:"note"`
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDigestDeterministic(t *testing.T) {
	root := writeTree(t, map[string]string{"b.mp4": "BBBB", "a.mp4": "AAAA"})
	k1, err := KeyFor(root, "cfg")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := KeyFor(root, "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("digest not deterministic")
	}
}

func TestDigestChangesWithInputConfigVersion(t *testing.T) {
	root := writeTree(t, map[string]string{"a.mp4": "AAAA"})
	base, _ := KeyFor(root, "cfg")
	if k, _ := KeyFor(root, "cfg2"); k == base {
		t.Fatal("config change must change key")
	}
	if err := os.WriteFile(filepath.Join(root, "a.mp4"), []byte("AAAB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k, _ := KeyFor(root, "cfg"); k == base {
		t.Fatal("input change must change key")
	}
	files, _ := HashTree(root)
	d1 := Digest(DigestInput{Files: files, Config: "cfg", ParserVersion: ParserVersion})
	d2 := Digest(DigestInput{Files: files, Config: "cfg", ParserVersion: ParserVersion + 1})
	if d1 == d2 {
		t.Fatal("parser version change must change key")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	want := payload{Tracks: 3, Note: "ok"}
	if err := s.Save("idx", "key1", want); err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := s.Load("idx", "key1", &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestStoreStaleOnKeyMismatch(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save("idx", "key1", payload{}); err != nil {
		t.Fatal(err)
	}
	var got payload
	err := s.Load("idx", "key2", &got)
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeIndexStale || !fe.Recoverable {
		t.Fatalf("want recoverable stale fault, got %v", err)
	}
}

func TestStoreCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, "idx.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got payload
	err := s.Load("idx", "k", &got)
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeIndexCorrupt {
		t.Fatalf("want corrupt fault, got %v", err)
	}
}

func TestEmptyInputRejected(t *testing.T) {
	if _, err := KeyFor(t.TempDir(), "c"); err == nil {
		t.Fatal("empty input must be rejected")
	}
}
