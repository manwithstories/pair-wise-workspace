package media

import (
	"strings"
	"testing"
)

func TestErrorLocatability(t *testing.T) {
	e := NewError("parser", KindChecksum, 128, ActionGap, "sha256 mismatch")
	if !strings.Contains(e.Error(), "offset=128") || !strings.Contains(e.Error(), "checksum_mismatch") {
		t.Fatalf("error not locatable: %v", e)
	}
	s := NewSeqError("rtp", "missing_packet", 42, ActionRecovered, "xor rebuild")
	if !strings.Contains(s.Error(), "seq=42") || !s.HasSequence {
		t.Fatalf("seq error not locatable: %v", s)
	}
}

func TestRecoveryActionStrings(t *testing.T) {
	cases := map[RecoveryAction]string{
		ActionFail:          "fail",
		ActionTruncate:      "truncate",
		ActionGap:           "gap",
		ActionFallbackIndex: "index-fallback",
		ActionRecovered:     "recovered",
		ActionLimit:         "limit",
		ActionCancel:        "cancel",
	}
	for act, want := range cases {
		if got := act.String(); got != want {
			t.Fatalf("action %d = %q, want %q", act, got, want)
		}
	}
}
