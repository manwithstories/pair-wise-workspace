package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScale(t *testing.T) {
	cases := []struct {
		value, src, dst, want int64
	}{
		{1000, 1000, 90000, 90000}, // 1 s media ticks -> RTP clock
		{500, 1000, 1000, 500},
		{1, 3, 1000, 333}, // nearest, half away from zero
		{-1, 3, 1000, -333},
		{0, 1000, 90000, 0},
	}
	for _, c := range cases {
		if got := Scale(c.value, c.src, c.dst); got != c.want {
			t.Errorf("Scale(%d,%d,%d)=%d want %d", c.value, c.src, c.dst, got, c.want)
		}
	}
}

func TestRationalArithmetic(t *testing.T) {
	a := TicksR(1, 3)
	b := TicksR(1, 6)
	if got := a.Add(b); got.String() != "1/2" {
		t.Fatalf("add=%s", got)
	}
	if got := a.Sub(b); got.String() != "1/6" {
		t.Fatalf("sub=%s", got)
	}
	if got := MillisR(1500).Millis(); got != 1500 {
		t.Fatalf("millis=%d", got)
	}
	if MillisR(-5).IsNegative() != true {
		t.Fatal("negative detection failed")
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	good, err := SafeJoin(root, "rep1/init.mp4")
	if err != nil {
		t.Fatalf("safe join: %v", err)
	}
	if !within(good, realRoot) {
		t.Fatalf("result outside root: %s", good)
	}
	for _, ref := range []string{"../escape.mp4", "../../etc/passwd", "a/../../b"} {
		if _, err := SafeJoin(root, ref); err == nil {
			t.Errorf("ref %q should be rejected", ref)
		}
	}
	if _, err := SafeJoin(root, "/etc/passwd"); err == nil {
		t.Error("absolute ref should be rejected")
	}
}

func TestSafeJoinRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeJoin(root, "link.mp4"); err == nil {
		t.Fatal("symlink escaping root must be rejected")
	}
}

func TestRecorderAndFault(t *testing.T) {
	r := NewRecorder()
	r.Add(NewFault(FaultBurstLoss, SevDamaged, "gap", Source{RepID: "v0", SegmentIndex: 3}))
	if !r.HasDamaged() || len(r.All()) != 1 {
		t.Fatal("recorder state wrong")
	}
	var fe error = &FaultError{F: NewFault(FaultTruncatedBox, SevDamaged, "x", Source{})}
	if f, ok := AsFault(fe); !ok || f.Kind != FaultTruncatedBox {
		t.Fatal("fault extraction failed")
	}
}
