package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRationalArithmetic(t *testing.T) {
	a := NewRational(1, 3)
	b := NewRational(1, 6)
	if got := a.Add(b); got.Num != 1 || got.Den != 2 {
		t.Fatalf("add: got %s", got)
	}
	if got := a.Sub(b); got.Num != 1 || got.Den != 6 {
		t.Fatalf("sub: got %s", got)
	}
	if a.Cmp(b) <= 0 || b.Cmp(a) >= 0 || a.Cmp(a) != 0 {
		t.Fatal("cmp order broken")
	}
	if got := NewRational(-4, -8); got.Num != 1 || got.Den != 2 {
		t.Fatalf("sign reduce: got %s", got)
	}
	if got := NewRational(5, 0); got.Den == 0 {
		t.Fatal("zero denominator must be coerced")
	}
	if got := NewRational(1500, 1000).MillisRounded(); got != 1500 {
		t.Fatalf("millis: %d", got)
	}
}

func TestUnwrap33(t *testing.T) {
	cases := []struct {
		name string
		prev int64
		raw  int64
		want int64
		ok   bool
	}{
		{"simple", 1000, 2000, 2000, true},
		{"wrap forward", Wrap33 - 100, 50, Wrap33 + 50, true},
		{"wrap backward", 50, Wrap33 - 100, -100, true},
		{"exact half is ambiguous", 0, HalfWrap33, 0, false},
		{"near half resolves", 0, HalfWrap33 - 1, HalfWrap33 - 1, true},
	}
	for _, tc := range cases {
		got, ok := Unwrap33(tc.prev, tc.raw)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: got (%d,%v) want (%d,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMedianOffsetAndDiff(t *testing.T) {
	a := []Rational{Ticks(100, 1000), Ticks(120, 1000), Ticks(140, 1000)}
	v := []Rational{Ticks(90, 1000), Ticks(100, 1000), Ticks(130, 1000)}
	off, ok := MedianOffset(a, v)
	if !ok || off.MillisRounded() != 10 {
		t.Fatalf("median offset: %v %s", ok, off)
	}
	if d := MaxAbsDiff(a, v); d.MillisRounded() != 20 {
		t.Fatalf("max diff: %s", d)
	}
	if _, ok := MedianOffset(nil, nil); ok {
		t.Fatal("empty must not resolve")
	}
}

func TestResolveLocalURI(t *testing.T) {
	dir := "/tmp/cases/normal"
	good := []string{"seg0.ts", "sub/seg1.ts", "./seg2.ts", "a.b/c.ts"}
	for _, u := range good {
		if _, err := ResolveLocalURI(dir, u); err != nil {
			t.Errorf("%q rejected: %v", u, err)
		}
	}
	bad := []string{
		"", "http://example.com/a.ts", "https://example.com/a.ts", "file:///etc/passwd",
		"//example.com/a.ts", "/etc/passwd", "../escape.ts", "a/../../escape.ts",
		"data:text/plain,hi", "a\\b.ts", "\x00.ts",
	}
	for _, u := range bad {
		if _, err := ResolveLocalURI(dir, u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := ResolveLocalURI(dir, "https://x/a.ts"); !errors.Is(err, ErrUnsafeURI) {
		t.Fatalf("wrong error kind: %v", err)
	}
	if _, err := ResolveLocalURI(dir, "../x"); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("wrong traversal kind: %v", err)
	}
}

func TestTimelineAndFileLimits(t *testing.T) {
	if err := CheckTimelineLimit(Zero, NewRational(11*3600, 1)); err != nil {
		t.Fatalf("11h rejected: %v", err)
	}
	if err := CheckTimelineLimit(Zero, NewRational(12*3600+1, 1)); !errors.Is(err, ErrTimelineTooLong) {
		t.Fatalf("12h+1s accepted: %v", err)
	}
	path := filepath.Join(t.TempDir(), "seg.bin")
	writeFile(t, path, []byte("0123456789"))
	if _, err := ReadSegment(path, 2, 3); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSegment(path, 2, 3); err != nil || string(got) != "234" {
		t.Fatalf("range: %q %v", got, err)
	}
	if _, err := ReadSegment(path, 8, 5); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("out-of-range accepted: %v", err)
	}
}

func TestFileSizeLimitsEnforced(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxSegmentBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := ReadSegment(big, 0, 0); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized segment accepted: %v", err)
	}
	man := filepath.Join(dir, "big.m3u8")
	g, err := os.Create(man)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Truncate(MaxManifestBytes + 1); err != nil {
		t.Fatal(err)
	}
	g.Close()
	if _, err := ReadManifest(man); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized manifest accepted: %v", err)
	}
}
