package manifest

import (
	"path/filepath"
	"testing"
)

func TestCheckRefRejectsRemoteAndTraversal(t *testing.T) {
	bad := []string{
		"https://evil.example/a.m4s",
		"http://127.0.0.1/x",
		"file:///etc/passwd",
		"//cdn.example/x",
		"/etc/passwd",
		"../../etc/passwd",
		"a/../../../b",
		"",
	}
	for _, ref := range bad {
		if err := CheckRef(ref); err == nil {
			t.Fatalf("CheckRef(%q) accepted, want rejection", ref)
		}
	}
	good := []string{"seg_0.m4s", "av/seg_1.m4s", "./map.mp4", "a//b.m4s"}
	for _, ref := range good {
		if err := CheckRef(ref); err != nil {
			t.Fatalf("CheckRef(%q) rejected: %v", ref, err)
		}
	}
}

func TestResolveLocalStaysInsideBase(t *testing.T) {
	base := t.TempDir()
	got, err := ResolveLocal(base, "sub/seg.m4s")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("expected absolute joined path, got %s", got)
	}
	if _, err := ResolveLocal(base, "../../x"); err == nil {
		t.Fatal("escape path must be rejected")
	}
}

func TestIsRemote(t *testing.T) {
	if !IsRemote("HTTP://Example.COM/x") {
		t.Fatal("uppercase scheme should count as remote")
	}
	if IsRemote("segments/a.m4s") {
		t.Fatal("relative ref is not remote")
	}
}
