package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/model"
)

func videoSpecs(n int) []SampleSpec {
	s := make([]SampleSpec, n)
	for i := range s {
		s[i] = SampleSpec{Size: 40, DTSStep: 3000, Key: i%5 == 0} // 90kHz, 30fps
	}
	return s
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanValidMoovAfterMdat(t *testing.T) {
	b := BuildMP4(true, 90000, videoSpecs(12), false, func(i int) byte { return byte(i) })
	p := writeTemp(t, "cam.mp4", b)
	res, err := Scan(p)
	if err != nil {
		t.Fatalf("clean file must not error: %v", err)
	}
	idx := res.Index
	if len(idx.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", idx.Faults)
	}
	if idx.MoovBefore {
		t.Fatal("moov should follow mdat")
	}
	if len(idx.Tracks) != 1 || !idx.Tracks[0].IsVideo || idx.Tracks[0].Codec != "avc1" {
		t.Fatalf("track parse wrong: %+v", idx.Tracks)
	}
	tr := idx.Tracks[0]
	if len(tr.Samples) != 12 {
		t.Fatalf("sample count = %d", len(tr.Samples))
	}
	// first sample sits exactly at mdat payload start
	if tr.Samples[0].Offset != idx.MdatPayload {
		t.Fatalf("first sample offset %d != mdat payload %d",
			tr.Samples[0].Offset, idx.MdatPayload)
	}
	// second sample DTS = 3000/90000 s = 33.333...ms; DTS is cumulative, so
	// sample[1] carries two deltas (~66.67ms), sample[0] one (~33.33ms).
	want0 := model.ToNanoseconds(3000, 90000)
	if tr.Samples[0].DTS != want0 {
		t.Fatalf("first DTS = %d, want %d", tr.Samples[0].DTS, want0)
	}
	want1 := model.ToNanoseconds(6000, 90000)
	if tr.Samples[1].DTS != want1 {
		t.Fatalf("second DTS = %d, want %d", tr.Samples[1].DTS, want1)
	}
	if tr.Regression != -1 || tr.WrappedAt != -1 {
		t.Fatal("clean track must have no PTS fault")
	}
}

func TestScanMoovBeforeMdat(t *testing.T) {
	b := BuildMP4(true, 90000, videoSpecs(6), true, func(i int) byte { return byte(i) })
	p := writeTemp(t, "front.mp4", b)
	res, err := Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Index.MoovBefore {
		t.Fatal("moov should precede mdat")
	}
	if len(res.Index.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", res.Index.Faults)
	}
}

func TestScanTruncatedAtomExactOffset(t *testing.T) {
	b := BuildMP4(true, 90000, videoSpecs(8), false, func(i int) byte { return byte(i) })
	// truncate 10 bytes inside the trailing mdat region
	cut := len(b) - 10
	p := writeTemp(t, "trunc.mov", b[:cut])
	res, err := Scan(p)
	if err == nil {
		t.Fatal("truncated file must return an error")
	}
	if res == nil || res.Index == nil {
		t.Fatal("partial index must still be returned")
	}
	idx := res.Index
	if idx.FirstBadByte != int64(cut) {
		t.Fatalf("first_bad_offset = %d, want exact %d", idx.FirstBadByte, cut)
	}
	if f, ok := model.AsFault(err); !ok || f.Kind != model.FaultTruncatedAtom {
		t.Fatalf("err = %v, want truncated_atom fault", err)
	}
}

func TestScanOutOfBoundsSample(t *testing.T) {
	b := BuildMP4(true, 90000, videoSpecs(6), false, func(i int) byte { return byte(i) })
	// corrupt the stco chunk offset to point past end of file.
	// box layout at the fourCC position: size(4)+type(4)+full(4)+count(4)+offset(4)
	pos := findBoxPayload(b, "stco") + 4 + 4 // skip FullBox + entry count
	if pos+4 > len(b) {
		t.Fatalf("stco offset position %d out of file range %d", pos, len(b))
	}
	binary.BigEndian.PutUint32(b[pos:], uint32(len(b)+256))
	p := writeTemp(t, "oob.mp4", b)
	res, err := Scan(p)
	if err != nil {
		t.Fatalf("OOB is recorded as fault, not hard error: %v", err)
	}
	if res.Index.FirstBadByte < 0 {
		t.Fatal("OOB sample must set first_bad_offset")
	}
	found := false
	for _, f := range res.Index.Faults {
		if f.Kind == model.FaultOutOfBounds {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected out_of_bounds fault, got %+v", res.Index.Faults)
	}
}

func TestScanMissingMoov(t *testing.T) {
	b := BuildMP4(true, 90000, videoSpecs(4), false, func(i int) byte { return byte(i) })
	// keep only ftyp + mdat by cutting at the moov header
	moov := findBoxPayload(b, "moov") - 8
	p := writeTemp(t, "nomoov.mp4", b[:moov])
	res, err := Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Index.Faults {
		if f.Kind == model.FaultMissingIndex {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected missing_index fault, got %+v", res.Index.Faults)
	}
}

func TestScanPTSWraparoundAndRegression(t *testing.T) {
	// wraparound is validated end-to-end through the dedicated fixture path;
	// here only assert the detector's classification helper directly.
	samples := []Sample{
		{Index: 0, PTS: 0}, {Index: 1, PTS: 100},
	}
	raw := []int64{4294967290, 5} // wraps forward across 2^32
	tr := Track{Regression: -1, WrappedAt: -1}
	var faults []*model.Fault
	detectPTSFaults(samples, raw, &tr, &FileIndex{Name: "x"},
		func(f *model.Fault) { faults = append(faults, f) })
	if tr.WrappedAt != 1 || tr.Regression != -1 {
		t.Fatalf("wrap=%d regression=%d", tr.WrappedAt, tr.Regression)
	}
	// true regression: small backward step, not a modular wrap
	tr2 := Track{Regression: -1, WrappedAt: -1}
	detectPTSFaults(samples, []int64{100, 80}, &tr2,
		&FileIndex{Name: "x"}, func(f *model.Fault) {})
	if tr2.Regression != 1 || tr2.WrappedAt != -1 {
		t.Fatalf("regression=%d wrap=%d", tr2.Regression, tr2.WrappedAt)
	}
}

func TestStoreContentAddressed(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := BuildMP4(false, 48000,
		[]SampleSpec{{Size: 20, DTSStep: 1024}, {Size: 20, DTSStep: 1024}},
		true, func(i int) byte { return byte(i) })
	p := writeTemp(t, "a.aac.m4a", b)
	res1, err := store.GetOrScan(p)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := store.GetOrScan(p)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Index.SHA256 != res2.Index.SHA256 {
		t.Fatal("digest must be stable across scans")
	}
	// mutate one byte: digest changes and store must rebuild
	b[len(b)-1] ^= 0xFF
	p2 := writeTemp(t, "b.m4a", b)
	res3, err := store.GetOrScan(p2)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Index.SHA256 == res1.Index.SHA256 {
		t.Fatal("changed source must produce a new content key")
	}
	if cached, _ := store.Load(res1.Index.SHA256); cached == nil {
		t.Fatal("original index must remain cached under its own digest")
	}
}

// findBoxPayload returns the payload offset (after the 8-byte header) of
// the first top-level or nested atom carrying four, or -1 when absent.
func findBoxPayload(b []byte, four string) int {
	needle := []byte(four)
	for i := 0; i+8 <= len(b); i++ {
		if b[i+4] == needle[0] && string(b[i+4:i+8]) == four {
			return i + 8
		}
	}
	return -1
}
