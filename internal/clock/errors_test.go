package clock

import (
	"errors"
	"testing"
)

func TestClassifiedErrorFormat(t *testing.T) {
	err := NewError(CatSample, Span{Segment: "cam0", Interval: Interval{Start: 10, End: 20}},
		RecoveredResync, "truncated sample", errors.New("unexpected EOF"))
	got := err.Error()
	want := "sample cam0@[10,20): truncated sample: unexpected EOF [recovery=resync]"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if Classify(err) != CatSample {
		t.Fatal("classify lost category")
	}
	if !IsRecoverable(err) {
		t.Fatal("resync should be recoverable")
	}
}

func TestWithSpanAndRecovery(t *testing.T) {
	base := Errorf(CatTransport, Span{}, RecoveredNone, "seq hole")
	got := WithRecovery(WithSpan(base, Span{Segment: "rtp-v", Interval: Interval{Start: 5, End: 9}}),
		RecoveredKeyframeReq)
	var ce *Error
	if !errors.As(got, &ce) {
		t.Fatal("not an *Error")
	}
	if ce.Span.Segment != "rtp-v" || ce.Recovery != RecoveredKeyframeReq ||
		ce.Span.Interval.Start != 5 || ce.Category != CatTransport {
		t.Fatalf("unexpected %+v", ce)
	}
	// Original must be unmodified.
	if base.Recovery != RecoveredNone || base.Span.Segment != "" {
		t.Fatal("With* mutated the original")
	}
}

func TestUnclassifiedBecomesInternal(t *testing.T) {
	err := errors.New("boom")
	if Classify(err) != CatInternal {
		t.Fatal("want internal")
	}
	if Classify(nil) != "" {
		t.Fatal("nil must classify empty")
	}
}

func TestListDeduplicatesAndCounts(t *testing.T) {
	var l List
	span := Span{Segment: "a", Interval: Interval{Start: 0, End: 10}}
	l.Add(Errorf(CatSample, span, RecoveredSkipped, "bad sample"))
	l.Add(Errorf(CatSample, span, RecoveredSkipped, "bad sample")) // duplicate entry
	l.Add(Errorf(CatIndexGap, span, RecoveredPrefix, "gap"))
	l.Add(nil)
	if l.Len() != 2 {
		t.Fatalf("len=%d", l.Len())
	}
	counts := l.CountByCategory()
	if counts[CatSample] != 1 || counts[CatIndexGap] != 1 {
		t.Fatalf("counts=%v", counts)
	}
	if !l.AnyRecoverable() || l.AnyUnrecoverable() {
		t.Fatal("recoverability flags wrong")
	}
	l.Add(Errorf(CatRender, span, RecoveredNone, "fatal"))
	if !l.AnyUnrecoverable() {
		t.Fatal("expected unrecoverable")
	}
	if err := l.Err(); err == nil {
		t.Fatal("Err should be non-nil")
	}
}
