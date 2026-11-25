package manifest_test

import (
	"os"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/manifest"
)

func generate(t *testing.T, damage string) *fixture.Bundle {
	t.Helper()
	root := t.TempDir()
	b, err := fixture.Generate(fixture.Options{Root: root, Seed: 41, Damage: damage})
	if err != nil {
		t.Fatalf("generate fixture: %v", err)
	}
	return b
}

func TestDASHSegmentTimelineMultiRepresentation(t *testing.T) {
	b := generate(t, "")
	data, err := os.ReadFile(b.DASHPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ParseDASH(b.DASHPath, data)
	if err != nil {
		t.Fatalf("ParseDASH: %v", err)
	}
	if len(m.Representations) < 3 {
		t.Fatalf("representations=%d, want at least 3", len(m.Representations))
	}
	for _, rep := range m.Representations {
		if len(rep.Segments) != fixture.SegmentCount {
			t.Fatalf("rep %s segments=%d want %d", rep.ID, len(rep.Segments), fixture.SegmentCount)
		}
		wantDur := clock.Must(8, 15)
		if clock.Cmp(rep.Segments[0].Duration, wantDur) != 0 {
			t.Fatalf("duration=%s want %s", rep.Segments[0].Duration, wantDur)
		}
		for i, s := range rep.Segments {
			if s.Index != i || s.InitURI == "" {
				t.Fatalf("bad segment %+v", s)
			}
		}
	}
}

func TestHLSMultipleDiscontinuities(t *testing.T) {
	b := generate(t, "")
	data, err := os.ReadFile(b.HLSPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ParseHLS(b.HLSPath, data)
	if err != nil {
		t.Fatalf("ParseHLS: %v", err)
	}
	if len(m.Segments) != 9 {
		t.Fatalf("segments=%d want 9", len(m.Segments))
	}
	wantPeriods := []int{0, 0, 0, 1, 1, 1, 2, 2, 2}
	for i, s := range m.Segments {
		if s.Period != wantPeriods[i] {
			t.Fatalf("seg %d period=%d want %d", i, s.Period, wantPeriods[i])
		}
		if s.InitURI == "" {
			t.Fatalf("seg %d missing EXT-X-MAP", i)
		}
	}
}

func TestFMP4InitAndBoundaryValidation(t *testing.T) {
	b := generate(t, "")
	initData, err := os.ReadFile(b.Init)
	if err != nil {
		t.Fatal(err)
	}
	codecs, err := manifest.ValidateInit(b.Init, initData)
	if err != nil {
		t.Fatalf("ValidateInit: %v", err)
	}
	if len(codecs) < 2 {
		t.Fatalf("codecs=%v want avc1 and ipcm", codecs)
	}
	segData, err := os.ReadFile(b.Segments[0])
	if err != nil {
		t.Fatal(err)
	}
	frag, err := manifest.ParseFragment(b.Segments[0], segData)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	var firstMDAT manifest.Box
	for _, bx := range frag.Boxes {
		if bx.Type == "mdat" {
			firstMDAT = bx
			break
		}
	}
	if err := manifest.CheckAVCCLengths(b.Segments[0], segData, firstMDAT); err != nil {
		t.Fatalf("CheckAVCCLengths: %v", err)
	}
}

func TestTruncatedMDATStopsAtLocatedBox(t *testing.T) {
	b := generate(t, fixture.DamageTruncatedMDAT)
	data, err := os.ReadFile(b.Segments[0])
	if err != nil {
		t.Fatal(err)
	}
	_, err = manifest.ScanAllBoxes(b.Segments[0], data)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	f, ok := fault.As(err)
	if !ok || f.Code != fault.BoxTruncated {
		t.Fatalf("error=%v want E_BOX_TRUNCATED fault", err)
	}
	if f.BoxOffset != b.Damage.BoxOffset {
		t.Fatalf("box offset=%d want %d", f.BoxOffset, b.Damage.BoxOffset)
	}
	if f.Offset != b.Damage.ErrorOffset {
		t.Fatalf("error offset=%d want %d", f.Offset, b.Damage.ErrorOffset)
	}
}

func TestCorruptMDATStopsAtNALLength(t *testing.T) {
	b := generate(t, fixture.DamageCorruptMDAT)
	data, err := os.ReadFile(b.Segments[0])
	if err != nil {
		t.Fatal(err)
	}
	boxes, err := manifest.ScanAllBoxes(b.Segments[0], data)
	if err != nil {
		t.Fatalf("box scan should pass boundaries: %v", err)
	}
	var mdat manifest.Box
	for _, bx := range boxes {
		if bx.Type == "mdat" {
			mdat = bx
			break
		}
	}
	err = manifest.CheckAVCCLengths(b.Segments[0], data, mdat)
	f, ok := fault.As(err)
	if !ok || f.Code != fault.MdatCorrupt {
		t.Fatalf("error=%v want E_MDAT_CORRUPT", err)
	}
	if f.Offset != b.Damage.ErrorOffset {
		t.Fatalf("offset=%d want %d", f.Offset, b.Damage.ErrorOffset)
	}
}

func TestTruncatedFirstBoxStopsAtOffsetZero(t *testing.T) {
	b := generate(t, fixture.DamageTruncatedBox)
	data, err := os.ReadFile(b.Segments[0])
	if err != nil {
		t.Fatal(err)
	}
	_, err = manifest.ScanAllBoxes(b.Segments[0], data)
	f, ok := fault.As(err)
	if !ok || f.Code != fault.BoxTruncated {
		t.Fatalf("err=%v want E_BOX_TRUNCATED", err)
	}
	if f.BoxOffset != 0 || f.Offset != b.Damage.ErrorOffset {
		t.Fatalf("offsets box=%d error=%d", f.BoxOffset, f.Offset)
	}
}

func TestMissingDiscontinuityInitPropagatesToTimeline(t *testing.T) {
	b := generate(t, fixture.DamageMissingInit)
	data, err := os.ReadFile(b.HLSPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ParseHLS(b.HLSPath, data)
	if err != nil {
		t.Fatal(err)
	}
	tl := clock.NewTimeline("test")
	var found error
	for _, s := range m.Segments {
		err := tl.Observe(b.HLSPath, clock.Stamp{
			SegmentIndex: s.Index, Period: s.Period, PTS: s.Start, DTS: s.Start, Duration: s.Duration,
		}, s.InitURI)
		if err != nil {
			found = err
			break
		}
	}
	f, ok := fault.As(found)
	if !ok || f.Code != fault.InitMissing {
		t.Fatalf("found=%v want E_INIT_MISSING", found)
	}
}
