package fault

import (
	"errors"
	"sort"
	"testing"
)

func TestNewAndWithers(t *testing.T) {
	f := New(KindTruncatedBox, SeverityFatal, 2, 731, 4096, 12_345, "moov truncated")
	if f.FrameIndex != 731 || f.OffsetBytes != 4096 || f.Ticks != 12_345 {
		t.Fatalf("position fields wrong: %+v", f)
	}
	g := f.WithSpan(5, 128).WithName("video-main")
	if g.SpanFrames != 5 || g.SpanBytes != 128 || g.TrackName != "video-main" {
		t.Fatalf("withers lost state: %+v", g)
	}
	if f.SpanFrames != 0 {
		t.Fatalf("WithSpan must not mutate the receiver")
	}
	if g.Error() == "" {
		t.Fatalf("Error() must render a message")
	}
}

func TestFirstByKindAndPartition(t *testing.T) {
	fs := []Fault{
		New(KindRTPSeqGap, SeverityError, 1, 3, -1, 100, "gap"),
		New(KindKeyframeGap, SeverityWarning, 2, 9, 500, 200, "idr gap"),
		New(KindRTPSeqGap, SeverityError, 1, 7, -1, 300, "gap2"),
	}
	if got, ok := FirstByKind(fs, KindKeyframeGap); !ok || got.FrameIndex != 9 {
		t.Fatalf("FirstByKind wrong: %+v %v", got, ok)
	}
	if _, ok := FirstByKind(fs, KindAmbiguous); ok {
		t.Fatalf("absent kind must return false")
	}
	part := ByTrack(fs)
	if len(part[1]) != 2 || len(part[2]) != 1 {
		t.Fatalf("partition wrong: %+v", part)
	}
	// Relative order must be preserved within a track.
	if part[1][1].FrameIndex != 7 {
		t.Fatalf("order not preserved")
	}
}

func TestOrderKeyDeterministic(t *testing.T) {
	base := New(KindBadSample, SeverityError, 1, 10, 100, 500, "x")
	laterTick := base
	laterTick.Ticks = 501
	laterTrack := base
	laterTrack.TrackID = 2
	laterOffset := base
	laterOffset.OffsetBytes = 101
	laterFrame := base
	laterFrame.FrameIndex = 11
	if OrderKey(base, laterTick) != -1 || OrderKey(laterTick, base) != 1 {
		t.Fatal("ticks ordering")
	}
	if OrderKey(base, laterTrack) != -1 {
		t.Fatal("track tie-break")
	}
	if OrderKey(base, laterOffset) != -1 {
		t.Fatal("offset tie-break")
	}
	if OrderKey(base, laterFrame) != -1 {
		t.Fatal("frame tie-break")
	}
	if OrderKey(base, base) != 0 {
		t.Fatal("equal elements")
	}
	// Full total ordering on a shuffled slice: sort with OrderKey, then the
	// result must be strictly ordered and deterministic.
	slice := []Fault{laterFrame, laterTick, base, laterOffset, laterTrack}
	sort.SliceStable(slice, func(i, j int) bool {
		return OrderKey(slice[i], slice[j]) < 0
	})
	for i := 1; i < len(slice); i++ {
		if OrderKey(slice[i-1], slice[i]) > 0 {
			t.Fatalf("slice not sorted at %d", i)
		}
	}
	if OrderKey(slice[0], base) != 0 {
		t.Fatalf("smallest element should be base, got %+v", slice[0])
	}
}

func TestSeverityString(t *testing.T) {
	cases := map[Severity]string{
		SeverityInfo: "info", SeverityWarning: "warning",
		SeverityError: "error", SeverityFatal: "fatal",
	}
	for sev, want := range cases {
		if sev.String() != want {
			t.Fatalf("%d -> %s want %s", sev, sev.String(), want)
		}
	}
	var s Severity = 99
	if s.String() == "" {
		t.Fatal("unknown severity must still render")
	}
	if !errors.Is(ErrNoFaults, ErrNoFaults) {
		t.Fatal("sentinel error unusable")
	}
}
