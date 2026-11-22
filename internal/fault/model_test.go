package fault

import "testing"

func TestCheckOffset(t *testing.T) {
	if _, ok := CheckOffset("a.bin", -1, 10, StageMP4); ok {
		t.Fatal("negative offset must be rejected")
	}
	if _, ok := CheckOffset("a.bin", 10, 10, StageMP4); ok {
		t.Fatal("offset == size must be rejected")
	}
	if _, ok := CheckOffset("a.bin", 9, 10, StageMP4); !ok {
		t.Fatal("offset within bounds must pass")
	}
}

func TestCheckPath(t *testing.T) {
	good := []string{"clip/sample.srt", "sample.vtt", "a/b/../c.srt"}
	for _, g := range good {
		if _, err := CheckPath("/tmp/root", g); err != nil {
			t.Fatalf("path %q rejected: %v", g, err)
		}
	}
	bad := []string{"/etc/passwd", "../secret.srt", "../../x", "a/../../x"}
	for _, b := range bad {
		if _, err := CheckPath("/tmp/root", b); err == nil {
			t.Fatalf("path %q must be rejected", b)
		}
	}
}

func TestSafeAlloc(t *testing.T) {
	if _, err := SafeAlloc(0); err == nil {
		t.Fatal("zero allocation must error")
	}
	if _, err := SafeAlloc(MaxInputSize + 1); err == nil {
		t.Fatal("oversized allocation must error")
	}
	buf, err := SafeAlloc(4096)
	if err != nil || len(buf) != 4096 {
		t.Fatalf("valid allocation failed: %v", err)
	}
}

func TestFaultError(t *testing.T) {
	f := New("clip.mp4", 1234, StageMP4, true, "truncated %s", "box")
	if f.Offset != 1234 || !f.Recoverable || f.Stage != StageMP4 {
		t.Fatalf("fault fields wrong: %+v", f)
	}
	if s := f.Error(); s == "" {
		t.Fatal("fault must render as error string")
	}
}
