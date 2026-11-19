package index

import (
	"testing"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/stream"
)

// buildStream makes a stream of keyframe and non-keyframe NALs and returns the
// bytes plus the offsets of the keyframes it contains.
func buildStream(keyframes, total int) ([]byte, []int64) {
	var data []byte
	var kf []int64
	for i := 0; i < total; i++ {
		if i%total/keyframes == 0 && len(kf) < keyframes {
			kf = append(kf, int64(len(data)))
			data = append(data, 0, 0, 0, 1, 0x65, 0xAA, 0xBB)
			continue
		}
		data = append(data, 0, 0, 0, 1, 0x41, 0xCC, 0xDD)
	}
	return data, kf
}

func scanH264(t *testing.T, data []byte) *stream.Result {
	t.Helper()
	r, err := stream.Scan(stream.H264, data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseRoundTrip(t *testing.T) {
	entries := []Entry{
		{Sample: 0, Offset: 4, Size: 7, PTS: 0},
		{Sample: 1, Offset: 18, Size: 7, PTS: 3000},
	}
	data := Encode(90000, entries)
	p := Parse(data, "idx.bin")
	if len(p.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", p.Issues)
	}
	if p.Timescale != 90000 {
		t.Fatalf("timescale=%d", p.Timescale)
	}
	if len(p.Entries) != 2 || p.Entries[1].PTS != 3000 {
		t.Fatalf("entries=%+v", p.Entries)
	}
}

func TestParseTruncatedHeader(t *testing.T) {
	p := Parse([]byte{'I', 'D', 'X'}, "idx.bin")
	if !p.Issues.Has(errs.CodeIndexHeaderTruncated) {
		t.Fatalf("want truncated header, got %v", p.Issues)
	}
}

func TestParseBadMagic(t *testing.T) {
	p := Parse(make([]byte, 12), "idx.bin")
	if !p.Issues.Has(errs.CodeIndexHeaderBadMagic) {
		t.Fatalf("want bad magic, got %v", p.Issues)
	}
}

func TestParsePartialEntry(t *testing.T) {
	data := append(Encode(90000, nil), make([]byte, 10)...)
	p := Parse(data, "idx.bin")
	if !p.Issues.Has(errs.CodeIndexEntryOverrun) || !p.Truncated {
		t.Fatalf("want partial entry, got %v", p.Issues)
	}
}

func TestValidateOutOfRangeOffset(t *testing.T) {
	data, _ := buildStream(2, 8)
	scan := scanH264(t, data)
	bad := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: 0, Size: 7, PTS: 0},
		{Sample: 1, Offset: int64(len(data)) + 500, Size: 7, PTS: 3000},
	}}
	res := Validate(bad, scan, "idx.bin")
	if !res.Issues.Has(errs.CodeIndexOffsetOutOfRange) {
		t.Fatalf("want out-of-range, got %v", res.Issues)
	}
	if res.Dropped != 1 {
		t.Fatalf("dropped=%d want 1", res.Dropped)
	}
	for _, i := range res.Issues {
		if i.Code == errs.CodeIndexOffsetOutOfRange {
			if i.Loc.Offset != int64(len(data))+500 || i.Loc.Sample != 1 {
				t.Fatalf("location not preserved: %+v", i.Loc)
			}
		}
	}
}

func TestValidateMissingPageRepairedFromScan(t *testing.T) {
	data, kf := buildStream(3, 12)
	scan := scanH264(t, data)
	if len(kf) != 3 {
		t.Fatalf("fixture keyframes=%d", len(kf))
	}
	// Drop the middle keyframe from the index.
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[0], Size: 7, PTS: 0},
		{Sample: 2, Offset: kf[2], Size: 7, PTS: 60000},
	}}
	res := Validate(p, scan, "idx.bin")
	if res.Repaired == 0 {
		t.Fatalf("no repair performed: %+v", res)
	}
	if !res.Issues.Has(errs.CodeIndexMissingPage) {
		t.Fatalf("missing-page not classified: %v", res.Issues)
	}
	var repaired *RepairedEntry
	for i := range res.Entries {
		if res.Entries[i].Sample == 1 {
			repaired = &res.Entries[i]
		}
	}
	if repaired == nil {
		t.Fatal("sample 1 absent from repaired index")
	}
	if repaired.Source != SourceScan {
		t.Fatalf("source=%s want scan", repaired.Source)
	}
	// Originals keep their original provenance.
	for _, e := range res.Entries {
		if e.Sample == 0 && e.Source != SourceOriginal {
			t.Fatalf("original entry source changed: %+v", e)
		}
	}
}

func TestValidateDuplicateAndOverlap(t *testing.T) {
	data, kf := buildStream(2, 6)
	scan := scanH264(t, data)
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[0], Size: 7, PTS: 0},
		{Sample: 0, Offset: kf[1], Size: 7, PTS: 3000},
		{Sample: 1, Offset: kf[0], Size: 7, PTS: 6000},
	}}
	res := Validate(p, scan, "idx.bin")
	if !res.Issues.Has(errs.CodeIndexDuplicateSample) {
		t.Fatalf("duplicate not reported: %v", res.Issues)
	}
	if !res.Issues.Has(errs.CodeIndexOffsetOverlap) {
		t.Fatalf("overlap not reported: %v", res.Issues)
	}
}

func TestValidatePendingReviewOnTruncatedIndex(t *testing.T) {
	data, kf := buildStream(4, 12)
	scan := scanH264(t, data)
	p := &Parsed{Truncated: true, Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[0], Size: 7, PTS: 0},
	}}
	p.issues.Add(errs.ClassTruncated, errs.CodeIndexEntryOverrun, "cut", errs.UnknownLocation())
	res := Validate(p, scan, "idx.bin")
	if res.Pending == 0 {
		t.Fatalf("pending review not recorded: %+v", res)
	}
	if !res.Issues.Has(errs.CodeIndexPendingReview) {
		t.Fatalf("pending classification missing: %v", res.Issues)
	}
}

func TestValidatePTSRegressed(t *testing.T) {
	data, kf := buildStream(2, 6)
	scan := scanH264(t, data)
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[0], Size: 7, PTS: 9000},
		{Sample: 1, Offset: kf[1], Size: 7, PTS: 3000},
	}}
	res := Validate(p, scan, "idx.bin")
	if !res.Issues.Has(errs.CodeIndexPTSRegressed) {
		t.Fatalf("PTS regression not reported: %v", res.Issues)
	}
}

func TestValidateRequiresScan(t *testing.T) {
	res := Validate(&Parsed{}, nil, "idx.bin")
	if !res.Issues.Has(errs.CodeDependencyMissing) {
		t.Fatalf("missing dependency not reported: %v", res.Issues)
	}
}

func TestParseOversizedRejected(t *testing.T) {
	p := Parse(make([]byte, MaxIndexSize+1), "idx.bin")
	if !p.Issues.Has(errs.CodeIndexTooLarge) {
		t.Fatalf("oversized index accepted: %v", p.Issues)
	}
}

func TestValidateClampsWildSampleNumber(t *testing.T) {
	data, kf := buildStream(2, 6)
	scan := scanH264(t, data)
	// A tiny index whose sample number claims ~4 billion pages. It must be
	// clamped to the scanned switch points, not allocated.
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0xFFFFFFFE, Offset: kf[0], Size: 7, PTS: 0},
	}}
	res := Validate(p, scan, "idx.bin")
	if len(res.Entries) > len(scan.Units)+len(kf)+8 {
		t.Fatalf("repaired index exploded to %d entries", len(res.Entries))
	}
	if !res.Issues.Has(errs.CodeIndexOffsetOutOfRange) {
		t.Fatalf("wild claim not reported: %v", res.Issues)
	}
}

func TestValidateDropsOutOfOrderOriginal(t *testing.T) {
	data, kf := buildStream(2, 6)
	scan := scanH264(t, data)
	// Sample 0 points at the later keyframe, sample 1 at the earlier one.
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[1], Size: 7, PTS: 0},
		{Sample: 1, Offset: kf[0], Size: 7, PTS: 3000},
	}}
	res := Validate(p, scan, "idx.bin")
	if res.Dropped == 0 {
		t.Fatalf("out-of-order entries not dropped: %+v", res.Entries)
	}
	// The kept/rep aired index must be non-decreasing in offset.
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i].Offset >= 0 && res.Entries[i-1].Offset >= 0 &&
			res.Entries[i].Offset < res.Entries[i-1].Offset {
			t.Fatalf("repaired index still out of order: %+v", res.Entries)
		}
	}
}

func TestValidateExtrapolationNothing(t *testing.T) {
	// Regression: an originally-ordered index keeps both entries.
	data, kf := buildStream(2, 6)
	scan := scanH264(t, data)
	p := &Parsed{Timescale: 90000, Entries: []Entry{
		{Sample: 0, Offset: kf[0], Size: 7, PTS: 0},
		{Sample: 1, Offset: kf[1], Size: 7, PTS: 3000},
	}}
	res := Validate(p, scan, "idx.bin")
	if res.Dropped != 0 {
		t.Fatalf("ordered entries dropped: %v", res.Issues)
	}
}
