package main

import (
	"testing"

	"pairflow/internal/clock"
)

func TestErrorExitMapping(t *testing.T) {
	cases := []struct {
		kind clock.ErrorKind
		want int
	}{
		{clock.KindIndexSeek, exitVerifyError},
		{clock.KindManifestAlignment, exitVerifyError},
		{clock.KindCheckpointMismatch, exitVerifyError},
		{clock.KindIndeterminate, exitMediaError},
		{clock.KindInputTooLarge, exitInputError},
		{clock.KindPathEscape, exitInputError},
		{clock.KindUnsupportedPayload, exitInputError},
		{clock.KindTruncatedHeader, exitInputError},
		{clock.KindBurstLoss, exitMediaError},
	}
	for _, c := range cases {
		err := clock.New("test", c.kind, "dev", 123, 9, "m", "r")
		if got := mapErrorToExit(err); got != c.want {
			t.Errorf("kind=%s got exit %d want %d", c.kind, got, c.want)
		}
	}
}

func TestStageErrorCarriesLocation(t *testing.T) {
	err := clock.New("wav.parse", clock.KindTruncatedHeader, "dev-c", 4804, 20030,
		"wav header truncated", "请求重传该源块")
	se, ok := clock.AsStageError(err)
	if !ok {
		t.Fatal("must unwrap to StageError")
	}
	if se.Stage != "wav.parse" || se.Offset != 4804 || se.Seq != 20030 ||
		se.DeviceID != "dev-c" || se.Recovery == "" {
		t.Fatalf("structured location lost: %+v", se)
	}
	if se.Kind != clock.KindTruncatedHeader {
		t.Fatalf("kind mismatch: %s", se.Kind)
	}
}
