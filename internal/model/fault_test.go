package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFaultFaultsSortAndFirst(t *testing.T) {
	var fs Faults
	fs.Add(Fault{Kind: FaultAVSync, Severity: SeverityWarning, TimeStart: MustRational(2, 1), IndexEntry: 2})
	fs.Add(Fault{Kind: FaultMissingSegment, Severity: SeverityCritical, TimeStart: MustRational(1, 1), IndexEntry: 1})
	fs.Add(Fault{Kind: FaultMissingSegment, Severity: SeverityCritical, TimeStart: MustRational(3, 1), IndexEntry: 3})
	fs.Sort()
	all := fs.All()
	if len(all) != 3 || all[0].IndexEntry != 1 || all[2].IndexEntry != 3 {
		t.Fatalf("sort failed: %+v", all)
	}
	first, err := fs.First(FaultMissingSegment)
	if err != nil {
		t.Fatal(err)
	}
	if first.IndexEntry != 1 {
		t.Fatalf("first missing should be entry 1, got %d", first.IndexEntry)
	}
	if fs.Counts()[FaultMissingSegment] != 2 || !fs.Has(FaultAVSync) {
		t.Fatalf("counts: %v", fs.Counts())
	}
	if _, err := fs.First(FaultPacketLoss); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestFaultValidateRanges(t *testing.T) {
	good := Fault{Kind: FaultTruncatedBox, Severity: SeverityCritical,
		ByteStart: 10, ByteEnd: 20, TimeStart: Zero, TimeEnd: MustRational(1, 1)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.ByteEnd = 5
	if err := bad.Validate(); err == nil {
		t.Fatal("inverted byte range accepted")
	}
	bad = good
	bad.Kind = "nope"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown kind accepted")
	}
	bad = good
	bad.TimeStart = MustRational(2, 1)
	if err := bad.Validate(); err == nil {
		t.Fatal("inverted time range accepted")
	}
}

func TestFaultJSONAndSummaryDeterministic(t *testing.T) {
	f := Fault{Kind: FaultTruncatedBox, Severity: SeverityCritical, Segment: "seg1.m4s",
		Message: "mdat declares more bytes than remain", ByteStart: 100, ByteEnd: 199,
		TimeStart: MustRational(1, 2), TimeEnd: MustRational(3, 2)}
	first, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("fault JSON is not deterministic")
	}
	var back Fault
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatal(err)
	}
	if !back.TimeStart.Equal(f.TimeStart) {
		t.Fatalf("rational round trip: %s", back.TimeStart)
	}
	summary := f.Summary()
	for _, want := range []string{"truncated_box", "seg1.m4s", "bytes=100..199", "t=500ms"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
}
