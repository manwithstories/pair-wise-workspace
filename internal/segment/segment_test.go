package segment_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/segment"
)

func readSegment(t *testing.T, fault fixture.Fault, rel string) []byte {
	t.Helper()
	root := t.TempDir()
	if err := fixture.Generate(root, fault); err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

func TestParseFragmentStructure(t *testing.T) {
	data := readSegment(t, fixture.FaultNone, "hls/seg0.m4s")
	frag, err := segment.ParseFragment(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantBoxes := []string{"styp", "moof", "mdat"}
	if len(frag.Boxes) != len(wantBoxes) {
		t.Fatalf("boxes = %v, want %v", frag.Boxes, wantBoxes)
	}
	for i, b := range wantBoxes {
		if frag.Boxes[i] != b {
			t.Fatalf("boxes = %v, want %v", frag.Boxes, wantBoxes)
		}
	}
	if !frag.HasTfdt || frag.BaseDecodeTime != 0 {
		t.Fatalf("tfdt = %d (present %v), want 0", frag.BaseDecodeTime, frag.HasTfdt)
	}
	if frag.TrackID != 1 {
		t.Fatalf("track id = %d, want 1", frag.TrackID)
	}
	if len(frag.Samples) != fixture.SamplesPerSeg {
		t.Fatalf("samples = %d, want %d", len(frag.Samples), fixture.SamplesPerSeg)
	}
	for i, s := range frag.Samples {
		if s.Duration != fixture.SampleDuration {
			t.Fatalf("sample %d duration = %d, want %d", i, s.Duration, fixture.SampleDuration)
		}
		wantSync := i%6 == 0
		if s.IsSync() != wantSync {
			t.Fatalf("sample %d sync = %v, want %v", i, s.IsSync(), wantSync)
		}
	}
	if frag.MdatOffset <= 0 || frag.MdatOffset+frag.MdatSize != int64(len(data)) {
		t.Fatalf("mdat span %d+%d does not reach EOF %d", frag.MdatOffset, frag.MdatSize, len(data))
	}
}

func TestTfdtValues(t *testing.T) {
	data := readSegment(t, fixture.FaultNone, "hls/seg3.m4s")
	frag, err := segment.ParseFragment(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if frag.BaseDecodeTime != 3*fixture.SegDuration {
		t.Fatalf("tfdt = %d, want %d", frag.BaseDecodeTime, 3*fixture.SegDuration)
	}
	// Rep b carries a presentationTimeOffset of 90000.
	data = readSegment(t, fixture.FaultNone, "dash/b/seg2.m4s")
	frag, err = segment.ParseFragment(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if frag.BaseDecodeTime != 90000+2*fixture.SegDuration {
		t.Fatalf("rep b tfdt = %d, want %d", frag.BaseDecodeTime, 90000+2*fixture.SegDuration)
	}
}

func TestFindKeyframes(t *testing.T) {
	data := readSegment(t, fixture.FaultNone, "dash/a/seg1.m4s")
	frag, err := segment.ParseFragment(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mdat := data[frag.MdatOffset : frag.MdatOffset+frag.MdatSize]
	kf, err := segment.FindKeyframes(mdat, frag.Samples, segment.CodecAVC)
	if err != nil {
		t.Fatalf("keyframes: %v", err)
	}
	if len(kf) != 2 || kf[0] != 0 || kf[1] != 6 {
		t.Fatalf("keyframes = %v, want [0 6]", kf)
	}
	gaps := segment.KeyframeGaps(kf)
	if len(gaps) != 1 || gaps[0] != 6 {
		t.Fatalf("gaps = %v, want [6]", gaps)
	}
}

func TestTruncatedBoxDetected(t *testing.T) {
	data := readSegment(t, fixture.FaultTruncatedBox, "hls/seg4.m4s")
	_, err := segment.ParseFragment(data)
	if err == nil {
		t.Fatal("truncated segment must fail parsing")
	}
	if !segment.IsTruncated(err) {
		t.Fatalf("error = %v, want TruncatedError", err)
	}
}

func TestSplitAVCCAndIDR(t *testing.T) {
	// 4-byte length + NAL, twice.
	sample := []byte{0, 0, 0, 3, 0x65, 0x88, 0x99, 0, 0, 0, 2, 0x41, 0x9a}
	nals, err := segment.SplitAVCC(sample)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(nals) != 2 {
		t.Fatalf("nals = %d, want 2", len(nals))
	}
	if !segment.IsIDR(segment.CodecAVC, nals[0]) {
		t.Fatal("nal 0 should be AVC IDR")
	}
	if segment.IsIDR(segment.CodecAVC, nals[1]) {
		t.Fatal("nal 1 should not be IDR")
	}
	// HEVC: nal_unit_type lives in bits 1..6 of the first byte.
	if !segment.IsIDR(segment.CodecHEVC, []byte{19 << 1, 0x01}) {
		t.Fatal("HEVC type 19 should be IDR")
	}
	if segment.IsIDR(segment.CodecHEVC, []byte{1 << 1, 0x01}) {
		t.Fatal("HEVC type 1 should not be IDR")
	}
	// Malformed: declared length overruns the buffer.
	if _, err := segment.SplitAVCC([]byte{0, 0, 0, 9, 0x65}); err == nil {
		t.Fatal("overrun NAL must fail")
	}
	if _, err := segment.SplitAVCC([]byte{0, 0}); err == nil {
		t.Fatal("dangling bytes must fail")
	}
}

func TestKeyframeOverrunMdat(t *testing.T) {
	data := readSegment(t, fixture.FaultNone, "hls/seg0.m4s")
	frag, err := segment.ParseFragment(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mdat := data[frag.MdatOffset : frag.MdatOffset+frag.MdatSize]
	// Cut the mdat payload so the last sample overruns.
	short := mdat[:len(mdat)-4]
	_, err = segment.FindKeyframes(short, frag.Samples, segment.CodecAVC)
	if err == nil || !segment.IsTruncated(err) {
		t.Fatalf("overrun err = %v, want TruncatedError", err)
	}
}
