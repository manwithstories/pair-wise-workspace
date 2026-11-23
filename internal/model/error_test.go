package model

import (
	"errors"
	"testing"
)

func TestErrorClassification(t *testing.T) {
	transient := New(KindTransient, "rtp", "jitter", "buffer underrun")
	if !IsRetryable(transient) {
		t.Fatal("transient error must be retryable")
	}
	fatal := New(KindCorrupt, "ingest", "parse", "truncated box")
	if IsRetryable(fatal) {
		t.Fatal("corrupt error must not be retryable")
	}
	wrapped := Wrap(KindChecksum, "verify", "hash", "seg-1", errors.New("mismatch"))
	if !errors.Is(wrapped, wrapped.Cause) {
		t.Fatal("Unwrap must expose the cause")
	}
	if me, ok := AsError(wrapped); !ok || me.Stage != "verify" {
		t.Fatalf("AsError failed: %#v", me)
	}
	if got := wrapped.Error(); got == "" || got == "mismatch" {
		t.Fatalf("Error() must carry classification, got %q", got)
	}
}
