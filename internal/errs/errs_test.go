package errs

import (
	"strings"
	"testing"
)

func TestLocationString(t *testing.T) {
	l := Location{File: "a.mp4", TrackID: 2, Kind: "video", Sample: 7, Offset: 128}
	got := l.String()
	for _, want := range []string{"a.mp4", "track=2", "sample=7", "offset=128"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
	if UnknownLocation().String() != "n/a" {
		t.Fatalf("unknown location string=%q", UnknownLocation().String())
	}
}

func TestListSortIsDeterministic(t *testing.T) {
	var l List
	l.Add(ClassCorrupt, "b.code", "second", Location{TrackID: 2, Sample: -1, Offset: 99})
	l.Add(ClassCorrupt, "a.code", "first", Location{TrackID: 1, Sample: 3, Offset: 10})
	l.Add(ClassTruncated, "c.code", "third", Location{TrackID: 1, Sample: 0, Offset: 10})
	l.Sort()
	if l[0].Loc.TrackID != 1 || l[0].Loc.Sample != 0 {
		t.Fatalf("first=%+v", l[0])
	}
	if l[2].Loc.TrackID != 2 {
		t.Fatalf("last=%+v", l[2])
	}
}

func TestCountsAndFatalTracking(t *testing.T) {
	var l List
	l.Add(ClassCorrupt, "x", "one", Location{Sample: -1, Offset: -1})
	l.AddFatal(ClassDependency, "y", "two", Location{TrackID: 4, Sample: -1, Offset: -1})
	l.Add(ClassDependency, "z", "three", Location{TrackID: 5, Sample: -1, Offset: -1})
	counts := l.CountByClass()
	if counts[string(ClassCorrupt)] != 1 || counts[string(ClassDependency)] != 2 {
		t.Fatalf("counts=%v", counts)
	}
	if !l.HasFatal() || !l.Has("y") || l.Has("nope") {
		t.Fatal("issue lookups wrong")
	}
	ft := l.FatalTracks()
	if len(ft) != 1 || ft[0] != 4 {
		t.Fatalf("fatal tracks=%v", ft)
	}
	if s := l.Summarize(); !strings.Contains(s, "3 issues") || !strings.Contains(s, "x") {
		t.Fatalf("summary=%q", s)
	}
}

func TestEmptyList(t *testing.T) {
	var l List
	if l.HasFatal() || l.Has("x") || l.Summarize() != "no issues" {
		t.Fatal("empty list behavior wrong")
	}
}

func TestIssueErrorIncludesLocation(t *testing.T) {
	i := Issue{Code: "index.offset_out_of_range", Message: "bad",
		Loc: Location{File: "f", Sample: 2, Offset: 44}}
	if s := i.Error(); !strings.Contains(s, "index.offset_out_of_range") || !strings.Contains(s, "offset=44") {
		t.Fatalf("error=%q", s)
	}
	j := Issue{Code: "plain", Message: "msg", Loc: UnknownLocation()}
	if j.Error() != "plain: msg" {
		t.Fatalf("plain error=%q", j.Error())
	}
	// A zero-value location is normalized to the -1 sentinels by Add.
	var l List
	l.Add(ClassCorrupt, "z", "m", Location{})
	if l[0].Loc.Sample != -1 || l[0].Loc.Offset != -1 {
		t.Fatalf("zero location not normalized: %+v", l[0].Loc)
	}
	// Sample 0 is reported as-is whenever any other location field is set.
	l.Add(ClassCorrupt, "y", "m", Location{File: "f", Sample: 0, Offset: 0})
	if l[1].Loc.Sample != 0 || l[1].Loc.Offset != 0 || l[1].Loc.File != "f" {
		t.Fatalf("sample 0 location altered: %+v", l[1].Loc)
	}
}
