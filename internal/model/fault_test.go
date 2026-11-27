package model

import (
	"errors"
	"testing"
)

func TestTrackParsing(t *testing.T) {
	for _, in := range []string{"audio", "AUDIO", " a "} {
		if tr, err := ParseTrack(in); err != nil || tr != TrackAudio {
			t.Fatalf("ParseTrack(%q) = %v, %v", in, tr, err)
		}
	}
	if tr, err := ParseTrack("video"); err != nil || tr != TrackVideo {
		t.Fatalf("ParseTrack(video) = %v, %v", tr, err)
	}
	if _, err := ParseTrack("subtitle"); err == nil {
		t.Fatal("expected error for unknown track")
	}
	if got := TrackVideo.String(); got != "video" {
		t.Fatalf("TrackVideo.String() = %q", got)
	}
}

func TestFaultSeverity(t *testing.T) {
	cases := map[FaultKind]Severity{
		FaultNone:            SeverityInfo,
		FaultSequenceWrap:    SeverityInfo,
		FaultDurationChange:  SeverityInfo,
		FaultDuplicate:       SeverityWarning,
		FaultTimestampRegres: SeverityWarning,
		FaultFECRecovered:    SeverityWarning,
		FaultLoss:            SeverityError,
		FaultTruncatedHeader: SeverityError,
		FaultBadLength:       SeverityError,
		FaultCorruptPayload:  SeverityError,
	}
	for kind, want := range cases {
		if got := kind.Severity(); got != want {
			t.Fatalf("%s severity = %d, want %d", kind, got, want)
		}
	}
	if FaultLoss.String() != "loss" || FaultKind(200).String() != "unknown" {
		t.Fatal("unexpected fault names")
	}
}

func TestDamageSetMergesOverlaps(t *testing.T) {
	var d DamageSet
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 100, End: 200}})
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 150, End: 250}})
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 400, End: 500}})
	// Touching ranges are merged too.
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 250, End: 400}})
	if d.Len() != 1 {
		t.Fatalf("ranges = %d, want 1 merged range: %v", d.Len(), d.Ranges())
	}
	r := d.Ranges()[0]
	if r.Start != 100 || r.End != 500 {
		t.Fatalf("merged range = %+v", r)
	}
	if r.Count != 4 {
		t.Fatalf("count = %d, want 4", r.Count)
	}
	if got := d.Total(); got != 400 {
		t.Fatalf("total = %d, want 400", got)
	}
}

func TestDamageSetMostSevereKindWins(t *testing.T) {
	var d DamageSet
	d.Add(Fault{Track: TrackVideo, Kind: FaultDuplicate, Slot: Slot{Start: 10, End: 20}})
	d.Add(Fault{Track: TrackVideo, Kind: FaultTruncatedHeader, Slot: Slot{Start: 15, End: 30}})
	if d.Len() != 1 || d.Ranges()[0].Kind != FaultTruncatedHeader {
		t.Fatalf("range = %+v, want truncated_header", d.Ranges())
	}
}

func TestDamageSetIgnoresInfoAndInvalid(t *testing.T) {
	var d DamageSet
	d.Add(Fault{Track: TrackAudio, Kind: FaultSequenceWrap, Slot: Slot{Start: 0, End: 100}})
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 100, End: 100}})
	d.Add(Fault{Track: TrackAudio, Kind: FaultLoss, Slot: Slot{Start: 300, End: 200}})
	d.Add(Fault{Track: TrackAudio, Kind: FaultNone, Slot: Slot{Start: 0, End: 300}})
	if d.Len() != 0 {
		t.Fatalf("expected empty damage set, got %v", d.Ranges())
	}
}

func TestDamageSetSortsByStart(t *testing.T) {
	var d DamageSet
	d.Add(Fault{Kind: FaultLoss, Slot: Slot{Start: 900, End: 950}})
	d.Add(Fault{Kind: FaultLoss, Slot: Slot{Start: 100, End: 120}})
	r := d.Ranges()
	if len(r) != 2 || r[0].Start != 100 || r[1].Start != 900 {
		t.Fatalf("ranges not sorted: %v", r)
	}
}

func TestSlotHelpers(t *testing.T) {
	a := Slot{Start: 0, End: 100}
	b := Slot{Start: 50, End: 200}
	c := Slot{Start: 200, End: 300}
	if !a.Overlaps(b) || b.Overlaps(c) || a.Overlaps(c) {
		t.Fatal("overlap detection wrong")
	}
	if got := a.Merge(b); got != (Slot{Start: 0, End: 200}) {
		t.Fatalf("merge = %+v", got)
	}
	if !a.Valid() || (Slot{Start: 5, End: 5}).Valid() {
		t.Fatal("validity wrong")
	}
	if got := (DamagedRange{Start: 10, End: 30, Kind: FaultLoss}).String(); got != "10-30:loss" {
		t.Fatalf("range string = %q", got)
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	for _, err := range []error{ErrCanceled, ErrLimitExceeded, ErrUnsupported, ErrDamaged} {
		if !errors.Is(err, err) {
			t.Fatal("sentinel identity broken")
		}
	}
	if errors.Is(ErrCanceled, ErrDamaged) {
		t.Fatal("sentinels must not alias")
	}
}
