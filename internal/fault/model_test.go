package fault

import (
	"errors"
	"fmt"
	"testing"

	"mediamerge/internal/clock"
)

func TestErrorFormattingAndUnwrap(t *testing.T) {
	span := clock.Span{Start: clock.MustRational(1, 2), End: clock.MustRational(3, 2)}
	cause := errors.New("short read")
	err := Wrap(StageParse, CodeTruncatedBox, cause, "box exceeds file")
	err.TrackID = 2
	err.Span = &span
	want := "parse: TRUNCATED_BOX track=2 range=1/2..3/2: box exceeds file: short read"
	if got := err.Error(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("cause not unwrapped")
	}
}

func TestLocatePreservesCauseAndFillsHoles(t *testing.T) {
	inner := New(StageParse, CodeNonMonotonic, "timestamps")
	span := clock.Span{Start: clock.MustRational(0, 1), End: clock.MustRational(1, 1)}
	located := Locate(fmt.Errorf("wrapped: %w", inner), StageValidate, 7, span)
	var fe *Error
	if !errors.As(located, &fe) {
		t.Fatalf("not a fault: %v", located)
	}
	if fe.TrackID != 7 || fe.Span == nil || fe.Code != CodeNonMonotonic || fe.Stage != StageParse {
		t.Fatalf("bad location %+v", fe)
	}
	if !errors.Is(located, inner) {
		t.Fatal("lost inner error")
	}

	plain := Locate(errors.New("disk full"), StageCommit, 3, span)
	if Classify(plain) != ClassFatal {
		t.Fatal("unknown error must classify fatal")
	}
	if Locate(nil, StageParse, 1, span) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Class
	}{
		{New(StageStream, CodeQueueFull, "full"), ClassRetryable},
		{New(StageCommit, CodeChecksumMismatch, "bad crc"), ClassCorrupt},
		{New(StageJournal, CodeWALGap, "gap"), ClassCorrupt},
		{New(StageParse, CodeCropOutOfBounds, "crop"), ClassFatal},
		{nil, ClassFatal},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
	if Classify(contextCanceled) != ClassRetryable {
		t.Fatal("context.Canceled should be retryable")
	}
}

func TestReport(t *testing.T) {
	var r Report
	r.Add(Diagnostic{Code: CodeColorMissing, Severity: SeverityWarn, Stage: StageResolve, TrackID: 1, Field: "primaries"})
	if r.HasErrors() {
		t.Fatal("warn must not block")
	}
	span := clock.Span{Start: clock.MustRational(0, 1), End: clock.MustRational(2, 1)}
	r.Add(Diagnostic{Code: CodeColorConflict, Severity: SeverityError, Stage: StageResolve, TrackID: 2, Field: "matrix", Chosen: "bt709", Rejected: []string{"bt2020"}, Span: &span, Message: "conflict"})
	if !r.HasErrors() {
		t.Fatal("error must block")
	}
	err := r.AsError(StageResolve)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeColorConflict || fe.TrackID != 2 || fe.Span == nil {
		t.Fatalf("bad error %v", err)
	}
	s := r.Items[0].String()
	if s != "warn COLOR_MISSING track=1 primaries" {
		t.Fatalf("diag string %q", s)
	}
	var empty Report
	if empty.AsError(StageResolve) != nil {
		t.Fatal("empty report")
	}
}
