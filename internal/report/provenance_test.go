package report

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"mediascan/internal/index"
	"mediascan/internal/model"
	"mediascan/internal/recover"
)

func fixedTime() time.Time { return time.Date(2026, 11, 22, 1, 2, 3, 0, time.UTC) }

func TestProvenanceJSONLDeterministic(t *testing.T) {
	work := t.TempDir()
	idx := &index.Index{Version: index.Version, InputSHA: "sha-input",
		Entries: []index.Entry{{Ordinal: 0, URI: "a.m4s", SHA256: "x", Present: true}}}
	cp := &recover.Checkpoint{NextSeg: 1, ManifestSHA: "sha-input"}
	faults := []model.Fault{
		{Kind: model.FaultMissingSegment, Severity: model.SeverityCritical,
			Segment: "seg-06.m4s", TimeStart: model.MustRational(12, 1), TimeEnd: model.MustRational(12, 1)},
		{Kind: model.FaultAVSync, Severity: model.SeverityCritical, Segment: "seg-07.m4s",
			Message: "max A/V skew 30ms", TimeStart: model.MustRational(7, 2), TimeEnd: model.MustRational(7, 2)},
	}
	run := func() []byte {
		p, err := New(work)
		if err != nil {
			t.Fatal(err)
		}
		p.Now = fixedTime
		p.Bind(idx, cp, "sha-result")
		if err := p.Write(Record{Kind: "run", Detail: "demo"}); err != nil {
			t.Fatal(err)
		}
		if err := p.WriteFaults(faults); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := run()
	second := run()
	if string(first) != string(second) {
		t.Fatalf("provenance output is not reproducible:\n%s\n%s", first, second)
	}
	lines := strings.Split(strings.TrimSpace(string(first)), "\n")
	if len(lines) != 4 { // run + 2 faults + summary
		t.Fatalf("lines=%d: %s", len(lines), first)
	}
	var summary Record
	if err := json.Unmarshal([]byte(lines[3]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Counts["missing_segment"] != 1 || summary.Counts["av_sync"] != 1 {
		t.Fatalf("counts: %+v", summary.Counts)
	}
	if summary.ManifestSHA != "sha-input" || summary.ResultSHA != "sha-result" ||
		summary.IndexVersion != index.Version || summary.Checkpoint != 1 {
		t.Fatalf("provenance fields: %+v", summary)
	}
}

func TestVerifyResultDetectsMismatch(t *testing.T) {
	work := t.TempDir()
	p, err := New(work)
	if err != nil {
		t.Fatal(err)
	}
	p.Now = fixedTime
	idx := &index.Index{Version: index.Version, InputSHA: "manifest-v1"}
	p.Bind(idx, nil, "result-1")
	if err := p.Write(Record{Kind: "summary"}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyResult(p.Path, "manifest-v1", "result-1"); err != nil {
		t.Fatalf("matching digests rejected: %v", err)
	}
	if err := VerifyResult(p.Path, "manifest-v2", "result-1"); err == nil {
		t.Fatal("input mismatch accepted")
	}
	if err := VerifyResult(p.Path, "manifest-v1", "result-2"); err == nil {
		t.Fatal("result mismatch accepted")
	}
}

func TestProvenanceRejectsInvalidFault(t *testing.T) {
	work := t.TempDir()
	p, err := New(work)
	if err != nil {
		t.Fatal(err)
	}
	bad := []model.Fault{{Kind: "unknown_fault", Severity: model.SeverityCritical}}
	if err := p.WriteFaults(bad); err == nil {
		t.Fatal("invalid fault written to provenance")
	}
}

func TestRereunTruncatesOldConclusions(t *testing.T) {
	work := t.TempDir()
	p, _ := New(work)
	p.Bind(&index.Index{Version: index.Version, InputSHA: "a"}, nil, "r1")
	if err := p.Write(Record{Kind: "summary"}); err != nil {
		t.Fatal(err)
	}
	p2, err := New(work)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p2.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		t.Fatal("new provenance file should be empty before the first write")
	}
}
