package fingerprint

import (
	"testing"
)

func seg(id string, start, end float64, samples int, silent bool, payload string) Segment {
	return Segment{
		ID: id, Track: "video", StartMs: start, EndMs: end,
		Samples: samples, Silent: silent, Payload: []byte(payload),
	}
}

func TestDuplicatesReportTimeRanges(t *testing.T) {
	segs := []Segment{
		seg("v#0", 0, 1000, 30, false, "payload-A"),
		seg("v#1", 1000, 2000, 30, false, "payload-A"), // identical
		seg("v#2", 2000, 3000, 30, false, "payload-B"),
	}
	dups := FindDuplicates(segs, 500)
	if len(dups) != 1 {
		t.Fatalf("duplicates=%d want 1", len(dups))
	}
	d := dups[0]
	if len(d.SegmentIDs) != 2 || d.SegmentIDs[0] != "v#0" || d.SegmentIDs[1] != "v#1" {
		t.Fatalf("segments=%v", d.SegmentIDs)
	}
	if len(d.TimeRanges) != 2 || d.TimeRanges[0] != [2]float64{0, 1000} ||
		d.TimeRanges[1] != [2]float64{1000, 2000} {
		t.Fatalf("ranges=%v", d.TimeRanges)
	}
}

func TestDurationIsPartOfFingerprint(t *testing.T) {
	// Same payload, different durations: must not merge.
	segs := []Segment{
		seg("a", 0, 1000, 30, false, "same"),
		seg("b", 1000, 2500, 45, false, "same"),
	}
	if dups := FindDuplicates(segs, 100); len(dups) != 0 {
		t.Fatalf("duration differs but merged: %+v", dups)
	}
}

func TestSilentSegmentsMergeOnlyAfterByteCheck(t *testing.T) {
	// Identical silent payloads: byte check passes → reported.
	segs := []Segment{
		seg("a#5", 2500, 3500, 10, true, string(make([]byte, 64))),
		seg("a#6", 3500, 4500, 10, true, string(make([]byte, 64))),
	}
	if dups := FindDuplicates(segs, 500); len(dups) != 1 {
		t.Fatalf("identical silent segments should verify and merge: %d", len(dups))
	}
	// Silent segments with different payloads have different digests
	// anyway; what matters is they never merge.
	segs[1].Payload = []byte("different-content-here-padding-to-64-bytes....................")
	if dups := FindDuplicates(segs, 500); len(dups) != 0 {
		t.Fatalf("different silent payloads merged: %+v", dups)
	}
}

func TestShortSegmentsNotMergedOnDigestAlone(t *testing.T) {
	// Short segments (< minDurationMs) with identical content merge
	// only via the byte-verified path; different content never does.
	short1 := seg("s1", 0, 200, 5, false, "short-payload")
	short2 := seg("s2", 200, 400, 5, false, "short-payload")
	if dups := FindDuplicates([]Segment{short1, short2}, 500); len(dups) != 1 {
		t.Fatalf("identical short segments should byte-verify and merge: %d", len(dups))
	}
	short2.Payload = []byte("other-payload")
	if dups := FindDuplicates([]Segment{short1, short2}, 500); len(dups) != 0 {
		t.Fatalf("short segments merged on digest alone: %+v", dups)
	}
}

func TestFingerprintStable(t *testing.T) {
	s := seg("x", 0, 1000, 30, false, "data")
	if Fingerprint(s) != Fingerprint(s) {
		t.Fatal("fingerprint not deterministic")
	}
}
