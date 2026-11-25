package scan

import (
	"bytes"
	"testing"

	"archivectl/internal/fault"
)

func testSpec(base uint64, v1 bool) FragmentSpec {
	return FragmentSpec{
		TrackID:        1,
		Sequence:       7,
		BaseDecodeTime: base, // non-zero start
		TFDTv1:         v1,
		Samples: []SampleSpec{
			{Duration: 3000, Size: 10, CTO: 6000}, // first -> sync
			{Duration: 3000, Size: 12, CTO: 3000},
			{Duration: 3000, Size: 11, CTO: 0},
		},
	}
}

func TestScanFragmentNonZeroStartAndPTS(t *testing.T) {
	b := BuildFragment(testSpec(9_000_000, true))
	rep, err := ScanReader(bytes.NewReader(b), "video")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Record.HasFatal() {
		t.Fatalf("unexpected faults: %v", rep.Record.Faults)
	}
	if len(rep.Fragments) != 1 {
		t.Fatalf("fragments=%d", len(rep.Fragments))
	}
	f := rep.Fragments[0]
	if f.TrackID != 1 || f.SequenceNumber != 7 {
		t.Fatalf("track/seq=%d/%d", f.TrackID, f.SequenceNumber)
	}
	if !f.HasTFDT || f.TFDT.Base != 9_000_000 || f.TFDT.ValueWidth != 8 {
		t.Fatalf("tfdt=%+v", f.TFDT)
	}
	if len(f.Samples) != 3 {
		t.Fatalf("samples=%d", len(f.Samples))
	}
	wantDTS := []int64{9_000_000, 9_003_000, 9_006_000}
	for i, s := range f.Samples {
		if s.DTS != wantDTS[i] {
			t.Fatalf("sample %d dts=%d want %d", i, s.DTS, wantDTS[i])
		}
		if want := wantDTS[i] + s.CompositionOffset; s.PTS != want {
			t.Fatalf("sample %d pts=%d want %d", i, s.PTS, want)
		}
	}
	if len(f.SyncSampleIndexes) != 1 || f.SyncSampleIndexes[0] != 0 {
		t.Fatalf("sync=%v want [0]", f.SyncSampleIndexes)
	}
	// data_offset must place first payload byte at a readable offset.
	if f.Samples[0].PayloadOffset < 0 || int(f.Samples[2].PayloadOffset)+11 > len(b) {
		t.Fatalf("payload offsets out of range: %+v", f.Samples)
	}
}

func TestScanFragmentV0WrappingBase(t *testing.T) {
	// A 32-bit tfdt sitting just below the wrap boundary.
	const nearWrap = uint64(0xFFFFFF00)
	b := BuildFragment(testSpec(nearWrap, false))
	rep, err := ScanReader(bytes.NewReader(b), "video")
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Fragments[0]
	if f.TFDT.Version != 0 || f.TFDT.ValueWidth != 4 || f.TFDT.Base != nearWrap {
		t.Fatalf("v0 tfdt decode wrong: %+v", f.TFDT)
	}
	// Raw DTS crosses the 32-bit boundary; repair must resolve the cycle,
	// scanner only reports the raw values.
	if f.Samples[2].DTS <= f.Samples[0].DTS {
		t.Fatal("raw cumulative DTS expected to run past the wrap point")
	}
}

func TestScanTruncatedTrunRecordedAsFatal(t *testing.T) {
	b := BuildFragment(testSpec(1000, true))
	cut := TruncateInsideTrun(b, 6) // only version/flags + part of count
	if cut == nil {
		t.Fatal("trun box not found by helper")
	}
	rep, err := ScanReader(bytes.NewReader(cut), "video")
	if err != nil {
		t.Fatal(err)
	}
	rec := rep.Record
	var found bool
	for _, fl := range rec.Faults {
		if fl.Code == "trun_truncated" && fl.Level == fault.LevelFatal && fl.Track == "video" {
			found = true
			if fl.Offset <= 0 {
				t.Fatal("truncation fault must carry a positive box offset")
			}
		}
	}
	if !found {
		t.Fatalf("expected trun_truncated fatal fault, got %+v", rec.Faults)
	}
}
