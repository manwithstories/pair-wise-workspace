package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pairwise/mediad/internal/media"
)

func TestWriteAtomicRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAtomic(filepath.Join("track-01", "seg-001.m4s"), []byte("fmp4")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read("track-01/seg-001.m4s")
	if err != nil || string(got) != "fmp4" {
		t.Fatalf("read=%q err=%v", got, err)
	}
}

func TestSafePathRejectsTraversalAndAbsolute(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../escape", "../../etc/passwd", "/tmp/escape"} {
		_, err := s.SafePath(rel)
		var me *media.Error
		if !errors.As(err, &me) || me.Code != media.CodePathViolation {
			t.Fatalf("rel=%q err=%v", rel, err)
		}
	}
}

func TestWriteRejectsOversizeBeforeDisk(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = s.WriteAtomic("big.m4s", make([]byte, MaxSegmentBytes+1))
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeTooLarge {
		t.Fatalf("err=%v", err)
	}
	if s.Exists("big.m4s") {
		t.Fatal("oversize file should not exist")
	}
}

func TestSemaphoreClampsToFour(t *testing.T) {
	sem := NewSemaphore(99)
	if cap(sem) != 4 {
		t.Fatalf("cap=%d", cap(sem))
	}
	if err := sem.Run(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(media.NewError(media.CodeLocalIO, "x", "", "").Error(), "field=") {
		t.Fatal("empty field should not be printed")
	}
}
