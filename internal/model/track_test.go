package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExitCodeContract(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{NewError(StageDemux, CodeTruncated, 10, 1, false, "bad box"), 2},
		{NewError(StageMapping, CodeMapping, -1, 1, false, "dup target"), 2},
		{NewError(StageDemux, CodeTimebase, -1, 1, false, "zero timescale"), 2},
		{NewError(StageStream, CodeCanceled, -1, 1, true, "canceled"), 3},
		{NewError(StageStream, CodeConsumerGone, -1, 1, true, "gone"), 3},
		{NewError(StageMux, CodeWrite, -1, 1, true, "write"), 3},
		{NewError(StageRecovery, CodeRecovery, -1, 0, true, "resume"), 3},
		{NewError(StageJournal, CodeJournal, -1, 0, false, "torn"), 4},
		{NewError(StageVerify, CodeIntegrity, -1, 0, false, "digest"), 4},
		{fmt.Errorf("plain"), 3},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Fatalf("ExitCode(%v)=%d want %d", c.err, got, c.want)
		}
	}
}

func TestErrorFieldsAreStable(t *testing.T) {
	b := Boundary{SampleIndex: 7, TrackID: 3, TrackSample: 7, OutputBytes: 4096}
	e := &Error{Stage: StageMux, Code: CodeWrite, Offset: 4096, TrackID: 3,
		Recoverable: true, Boundary: &b, Message: "disk full", Cause: errors.New("ENOSPC")}
	s := e.Error()
	for _, want := range []string{"stage=mux", "offset=4096", "track=3", "recoverable=true",
		"code=write", "disk full", "ENOSPC"} {
		if !strings.Contains(s, want) {
			t.Fatalf("error string %q missing %q", s, want)
		}
	}
	if !errors.Is(e, e.Cause) {
		t.Fatal("cause not unwrapable")
	}
}
