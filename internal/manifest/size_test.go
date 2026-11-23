package manifest

import (
	"errors"
	"testing"

	"mediarun/internal/clock"
)

func TestCheckSegmentSizeCeiling(t *testing.T) {
	l := Limits{MaxEntries: 10, MaxSegmentBytes: 256 << 20}
	if err := l.CheckSegment("seg", 1); err != nil {
		t.Fatalf("small segment rejected: %v", err)
	}
	err := l.CheckSegment("big", (256<<20)+1)
	var e *clock.Error
	if !errors.As(err, &e) || e.Code != clock.CodeSizeLimit {
		t.Fatalf("oversized segment not rejected with size code: %v", err)
	}
	if err := l.CheckSegment("neg", -1); err == nil {
		t.Fatalf("negative size accepted")
	}
}

func TestParseHLSBytesRejectsOversizedManifest(t *testing.T) {
	body := "#EXTM3U\n#EXTINF:4,\na.m4s\n"
	_, err := ParseHLSBytes([]byte(body),
		Limits{MaxEntries: 10, MaxSegmentBytes: 8})
	if err == nil {
		t.Fatalf("oversized manifest accepted")
	}
	if !clock.IsKind(err, clock.KindInvalid) {
		t.Fatalf("wrong kind: %v", err)
	}
}
