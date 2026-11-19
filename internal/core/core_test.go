package core

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorFormatsContext(t *testing.T) {
	e := &Error{Code: CodeBadIndex, Track: "cam-a", Detail: "offset out of range"}
	s := e.Error()
	for _, want := range []string{CodeBadIndex, "cam-a", "offset out of range"} {
		if !strings.Contains(s, want) {
			t.Errorf("Error() = %q, missing %q", s, want)
		}
	}
}

func TestSeqErrorRecordsSequence(t *testing.T) {
	e := SeqError(CodeBadPacket, "cam-a", 1234, "conflict")
	if e.Seq == nil || *e.Seq != 1234 {
		t.Fatalf("seq = %v", e.Seq)
	}
	if !strings.Contains(e.Error(), "seq=1234") {
		t.Fatalf("formatted error missing seq: %s", e.Error())
	}
}

func TestAsFindsWrappedError(t *testing.T) {
	orig := &Error{Code: CodeBadTimestamp, Track: "audio-1", Detail: "negative"}
	wrapped := errors.Join(errors.New("outer"), orig)
	got, ok := As(wrapped)
	if !ok || got.Code != CodeBadTimestamp {
		t.Fatalf("As = %+v, %v", got, ok)
	}
}

func TestCodeOf(t *testing.T) {
	if CodeOf(&Error{Code: CodeOverLimit}) != CodeOverLimit {
		t.Fatal("code not extracted")
	}
	if CodeOf(errors.New("plain")) != CodeInternal {
		t.Fatal("foreign errors must map to internal")
	}
}

func TestStampTrackFillsMissingTrack(t *testing.T) {
	e := &Error{Code: CodeBadIndex, Detail: "bad"}
	stamped := StampTrack(e, "cam-b")
	got, _ := As(stamped)
	if got.Track != "cam-b" {
		t.Fatalf("track = %q", got.Track)
	}
	if got.Code != CodeBadIndex {
		t.Fatalf("code changed: %q", got.Code)
	}
	// Existing track must win over the stamp.
	existing := &Error{Code: CodeBadIndex, Track: "cam-a"}
	got2, _ := As(StampTrack(existing, "cam-b"))
	if got2.Track != "cam-a" {
		t.Fatalf("stamp overwrote track: %q", got2.Track)
	}
}

func TestTrackedError(t *testing.T) {
	e := TrackedError(CodeAudioInvalid, "audio-2", "layout mismatch")
	if e.Track != "audio-2" || e.Code != CodeAudioInvalid || e.Fatal {
		t.Fatalf("unexpected: %+v", e)
	}
	if !strings.Contains(e.Error(), "audio-2") {
		t.Fatal("track missing from message")
	}
}
