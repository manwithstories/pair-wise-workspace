package packager

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/fingerprint"
	"mediacq/internal/split"
)

func mkVerified(t *testing.T, seq int, startMS, endMS int64, payload [][]byte, dupOf ...int) VerifiedSegment {
	t.Helper()
	fp := fingerprint.Compute(payload)
	seg := split.Segment{
		Seq: seq,
		Range: clock.Range{
			Start: clock.Time(startMS * int64(time.Millisecond)),
			End:   clock.Time(endMS * int64(time.Millisecond)),
		},
		EventIDs: []uint32{uint32(seq) + 1},
	}
	return VerifiedSegment{Segment: seg, Fingerprint: fp, Payload: payload}
}

func TestBuildAdmitsOnlyVerified(t *testing.T) {
	good := mkVerified(t, 0, 0, 500, [][]byte{{1, 2, 3}})
	good.Fingerprint = fingerprint.Compute(good.Payload)
	m, err := Build([]VerifiedSegment{good}, Options{TargetDuration: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 1 {
		t.Fatalf("segments = %d", len(m.Segments))
	}
	// An entry with no fingerprint evidence is refused.
	bad := VerifiedSegment{Segment: split.Segment{Seq: 1}}
	if _, err := Build([]VerifiedSegment{bad}, Options{}); !errors.Is(err, ErrUnverified) {
		t.Fatalf("want ErrUnverified, got %v", err)
	}
}

func TestBuildDeduplicates(t *testing.T) {
	a := mkVerified(t, 0, 0, 500, [][]byte{{7, 7, 7}})
	b := mkVerified(t, 1, 500, 1000, [][]byte{{8, 8, 8}})
	c := mkVerified(t, 2, 1000, 1500, [][]byte{{7, 7, 7}}) // duplicate content
	m, err := Build([]VerifiedSegment{a, b, c}, Options{TargetDuration: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Segments) != 2 || m.Duplicates != 1 {
		t.Fatalf("segments=%d duplicates=%d", len(m.Segments), m.Duplicates)
	}
}

func TestManifestAlignment(t *testing.T) {
	a := mkVerified(t, 0, 0, 490, [][]byte{{1}})
	b := mkVerified(t, 1, 490, 1005, [][]byte{{2}})
	m, err := Build([]VerifiedSegment{a, b}, Options{TargetDuration: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxAlignmentMS != 15 {
		t.Fatalf("max alignment = %d, want 15", m.MaxAlignmentMS)
	}
	for _, e := range m.Segments {
		if e.AlignmentMS < 0 || e.EndMS <= e.StartMS {
			t.Fatalf("bad entry %+v", e)
		}
	}
}

func TestManifestDeterministicJSON(t *testing.T) {
	a := mkVerified(t, 2, 0, 500, [][]byte{{1}})
	b := mkVerified(t, 0, 500, 1000, [][]byte{{2}})
	m1, _ := Build([]VerifiedSegment{a, b}, Options{TargetDuration: 500 * time.Millisecond})
	m2, _ := Build([]VerifiedSegment{a, b}, Options{TargetDuration: 500 * time.Millisecond})
	j1, _ := m1.MarshalJSON()
	j2, _ := m2.MarshalJSON()
	if string(j1) != string(j2) {
		t.Fatal("manifest JSON not deterministic")
	}
	var parsed map[string]any
	if err := json.Unmarshal(j1, &parsed); err != nil {
		t.Fatal(err)
	}
	// Entries must be seq-ordered regardless of input order.
	segs := parsed["segments"].([]any)
	first := segs[0].(map[string]any)
	if int(first["seq"].(float64)) != 0 {
		t.Fatal("manifest not ordered by seq")
	}
}

func TestResumeToken(t *testing.T) {
	a := mkVerified(t, 0, 0, 500, [][]byte{{1}})
	b := mkVerified(t, 1, 500, 1000, [][]byte{{2}})
	m, _ := Build([]VerifiedSegment{a, b}, Options{TargetDuration: 500 * time.Millisecond})
	tok, ok := m.ResumeFrom()
	if !ok || tok.LastSeq != 1 || tok.EndMS != 1000 {
		t.Fatalf("resume token = %+v ok=%v", tok, ok)
	}
}
