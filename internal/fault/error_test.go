package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorClassification(t *testing.T) {
	base := errors.New("unexpected EOF")
	rec := Recoverable(CodeTruncated, "mp4.ReadBox", 100, 200, base)
	if !IsRecoverable(rec) || IsFatal(rec) {
		t.Fatalf("recoverable misclassified: %v", rec)
	}
	if CodeOf(rec) != CodeTruncated {
		t.Fatalf("code = %q", CodeOf(rec))
	}
	if !errors.Is(rec, base) {
		t.Fatal("errors.Is must reach wrapped error")
	}
	if s := rec.Error(); s == "" || !contains(s, "range=[100,200)") || !contains(s, CodeTruncated) {
		t.Fatalf("error string missing detail: %q", s)
	}

	ft := Fatal(CodePathEscape, "cache.Open", errors.New("../etc"))
	if !IsFatal(ft) || IsRecoverable(ft) {
		t.Fatalf("fatal misclassified: %v", ft)
	}
	if CodeOf(fmt.Errorf("plain: %w", ft)) != CodePathEscape {
		t.Fatal("CodeOf must traverse fmt wrap")
	}
}

func TestWithRangePreservesFields(t *testing.T) {
	orig := New(KindRecoverable, CodeCorrupt, "bad stsz")
	scoped := WithRange(orig, 8, 16)
	if scoped.Kind != orig.Kind || scoped.Code != orig.Code || scoped.Msg != orig.Msg {
		t.Fatal("WithRange mutated shared fields")
	}
	if scoped.AffectedStart != 8 || scoped.AffectedEnd != 16 {
		t.Fatalf("range not applied: %+v", scoped)
	}
	// 原错误不得被修改。
	if orig.AffectedStart != 0 || orig.AffectedEnd != 0 {
		t.Fatal("WithRange mutated the receiver")
	}
}

func TestUnknownErrorIsNotRecoverable(t *testing.T) {
	if IsRecoverable(errors.New("plain")) {
		t.Fatal("plain error must not look recoverable")
	}
	if CodeOf(errors.New("plain")) != "" {
		t.Fatal("plain error code must be empty")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
