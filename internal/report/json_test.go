package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"archive.local/recover/internal/errs"
)

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name   string
		issues func(*errs.List)
		want   int
	}{
		{"clean", func(*errs.List) {}, ExitOK},
		{"corrupt", func(l *errs.List) {
			l.Add(errs.ClassCorrupt, "c", "m", errs.UnknownLocation())
		}, ExitPartial},
		{"truncated", func(l *errs.List) {
			l.Add(errs.ClassTruncated, "t", "m", errs.UnknownLocation())
		}, ExitPartial},
		{"dependency", func(l *errs.List) {
			l.AddFatal(errs.ClassDependency, "d", "m", errs.UnknownLocation())
		}, ExitUnrecoverable},
		{"cycle", func(l *errs.List) {
			l.AddFatal(errs.ClassCycle, "y", "m", errs.UnknownLocation())
		}, ExitUnrecoverable},
		{"digest", func(l *errs.List) {
			l.AddFatal(errs.ClassDigest, "g", "m", errs.UnknownLocation())
		}, ExitUnrecoverable},
	}
	for _, c := range cases {
		r := New("case", 1)
		c.issues(&r.Issues)
		r.Finalize()
		if r.ExitCode != c.want {
			t.Errorf("%s: exit=%d want %d", c.name, r.ExitCode, c.want)
		}
		if r.OK != (c.want == ExitOK) {
			t.Errorf("%s: ok=%v inconsistent with exit %d", c.name, r.OK, r.ExitCode)
		}
	}
}

func TestUsageCodePassesThrough(t *testing.T) {
	r := New("c", 1)
	r.ExitCode = ExitUsage
	r.Finalize()
	if r.ExitCode != ExitUsage || r.OK {
		t.Fatalf("exit=%d ok=%v", r.ExitCode, r.OK)
	}
}

func TestWriteIsStableJSON(t *testing.T) {
	r := New("demo", 17)
	r.Metrics = Metrics{SyncErrorMs: 1.5, LossRecoveryRate: 0.95, P95LatencyMs: 80, Segments: 30, AlignmentDeltaMs: 2}
	r.Issues.Add(errs.ClassCorrupt, "b", "second", errs.Location{TrackID: 2, Sample: 1, Offset: 5})
	r.Issues.Add(errs.ClassCorrupt, "a", "first", errs.Location{TrackID: 1, Sample: 0, Offset: 5})
	r.Finalize()

	var a, b bytes.Buffer
	if err := Write(&a, r); err != nil {
		t.Fatal(err)
	}
	if err := Write(&b, r); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Fatal("report output not deterministic")
	}
	var decoded map[string]any
	if err := json.Unmarshal(a.Bytes(), &decoded); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	for _, key := range []string{"schema_version", "metrics", "issues", "issue_counts", "exit_code", "ok"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing key %q", key)
		}
	}
	if !strings.HasSuffix(a.String(), "\n") {
		t.Fatal("output must end with a newline")
	}
}

func TestSortedIssueCodes(t *testing.T) {
	r := New("c", 1)
	r.Issues.Add(errs.ClassCorrupt, "z", "", errs.UnknownLocation())
	r.Issues.Add(errs.ClassCorrupt, "a", "", errs.UnknownLocation())
	r.Issues.Add(errs.ClassCorrupt, "a", "", errs.UnknownLocation())
	got := r.SortedIssueCodes()
	if len(got) != 2 || got[0] != "a" || got[1] != "z" {
		t.Fatalf("codes=%v", got)
	}
}
