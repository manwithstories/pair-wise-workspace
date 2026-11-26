package media

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestRatRescaleAndSeconds(t *testing.T) {
	got, err := Rescale(48000, NewRat(1, 48000), NewRat(1, 1000))
	if err != nil || got != 1000 {
		t.Fatalf("rescale = %d, %v", got, err)
	}
	if math.Abs(NewRat(1, 48000).Seconds(48000)-1) > 1e-12 {
		t.Fatal("one second mismatch")
	}
	if NewRat(2, 4).N != 1 || NewRat(2, 4).D != 2 {
		t.Fatal("rational was not reduced")
	}
}

func TestTimestampErrorCarriesStreamAndPosition(t *testing.T) {
	err := TimestampError(2, 1844, "PTS moved backwards")
	var me *Error
	if !errors.As(err, &me) {
		t.Fatal("expected media.Error")
	}
	if me.Stream != 2 || me.Position != 1844 || me.ExitCode() != 2 {
		t.Fatalf("bad timestamp error: %+v", me)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeJoin(root, "../escape"); err == nil {
		t.Fatal("expected parent traversal rejection")
	}
	if _, err := SafeJoin(root, "/etc/passwd"); err == nil {
		t.Fatal("expected absolute path rejection")
	}
	p, err := SafeJoin(root, filepath.Join("segments", "a.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel != filepath.Join("segments", "a.mp4") {
		t.Fatalf("unexpected safe path %q rel=%q err=%v", p, rel, err)
	}
}

func TestAtomicWriteAndHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "file.bin")
	n, sum, err := AtomicWriteFile(path, strings.NewReader("broadcast"), 0o644)
	if err != nil || n != 9 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	got, err := FileSHA256(path)
	if err != nil || got != sum {
		t.Fatalf("hash got=%q want=%q err=%v", got, sum, err)
	}
}
