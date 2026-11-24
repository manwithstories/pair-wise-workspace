package safepath

import (
	"path/filepath"
	"testing"
)

func TestJoinAllowsNested(t *testing.T) {
	root, err := NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := root.Join("staging", "track-1", "sample.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !root.Contains(p) {
		t.Fatal("nested path must be contained")
	}
}

func TestJoinRejectsEscape(t *testing.T) {
	root, err := NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Join("..", "..", "etc", "passwd"); err == nil {
		t.Fatal("parent-directory escape must be rejected")
	}
	abs, _ := filepath.Abs(filepath.Join(root.String(), "..", "outside"))
	if root.Contains(abs) {
		t.Fatal("sibling path must not be contained")
	}
}

func TestMustJoinPanicsOnViolation(t *testing.T) {
	root, _ := NewRoot(t.TempDir())
	defer func() {
		if recover() == nil {
			t.Fatal("MustJoin should panic on an escaping path")
		}
	}()
	root.MustJoin("..", "escape")
}
