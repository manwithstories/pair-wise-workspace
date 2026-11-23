package provenance

import (
	"encoding/json"
	"testing"
)

func sampleTrace(t *testing.T) *Trace {
	t.Helper()
	tr := NewTrace()
	in1 := HashPayload([]byte("seg-a"))
	mid := HashPayload([]byte("reordered"))
	out := HashPayload([]byte("final"))
	if err := tr.Record(Entry{
		Output: "art/reorder.json", OutputHash: mid,
		ManifestURI: "playlist.m3u8#a", InputHashes: []string{in1},
		Decisions: []Decision{{Stage: "rtp", Code: "reorder", Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Record(Entry{
		Output: "art/final.json", OutputHash: out,
		ManifestURI: "playlist.m3u8#a", InputHashes: []string{mid},
		SegmentIDs: []string{"cam0-000001"},
	}); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestTraceComplete(t *testing.T) {
	tr := sampleTrace(t)
	if !tr.Complete() {
		t.Fatalf("trace should be complete")
	}
	if len(tr.Entries()) != 2 {
		t.Fatalf("entries = %d", len(tr.Entries()))
	}
}

func TestTraceRejectsDuplicateAndIncomplete(t *testing.T) {
	tr := sampleTrace(t)
	e := tr.Entries()[0]
	if err := tr.Record(e); err == nil {
		t.Fatalf("duplicate entry accepted")
	}
	empty := NewTrace()
	if empty.Complete() {
		t.Fatalf("empty trace reported complete")
	}
	if err := empty.Record(Entry{Output: "x"}); err == nil {
		t.Fatalf("entry without hash accepted")
	}
}

func TestTraceLineageReachesManifest(t *testing.T) {
	tr := sampleTrace(t)
	out := HashPayload([]byte("final"))
	uris, err := tr.Lineage(out)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if len(uris) != 1 || uris[0] != "playlist.m3u8#a" {
		t.Fatalf("lineage = %v", uris)
	}
	if _, err := tr.Lineage("deadbeef"); err == nil {
		t.Fatalf("unknown output resolved")
	}
}

func TestTraceResolve(t *testing.T) {
	tr := sampleTrace(t)
	out := HashPayload([]byte("final"))
	got, err := tr.Resolve(out)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0].SegmentIDs[0] != "cam0-000001" {
		t.Fatalf("resolve = %+v", got)
	}
}

func TestTraceJSONRoundTrips(t *testing.T) {
	tr := sampleTrace(t)
	b, err := tr.JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var back struct {
		Entries  []Entry `json:"entries"`
		Complete bool    `json:"complete"`
		Summary  string  `json:"summary"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Complete || len(back.Entries) != 2 {
		t.Fatalf("round-trip lost data: %+v", back)
	}
	if back.Summary != tr.Summary() {
		t.Fatalf("summary mismatch")
	}
}

func TestTraceSummaryStable(t *testing.T) {
	a := sampleTrace(t)
	b := sampleTrace(t)
	if a.Summary() != b.Summary() {
		t.Fatalf("identical chains produced different summaries")
	}
}
