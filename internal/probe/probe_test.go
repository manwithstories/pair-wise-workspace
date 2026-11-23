package probe_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/probe"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestScanMP4Healthy(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	raw, err := spec.BuildMP4()
	if err != nil {
		t.Fatalf("BuildMP4: %v", err)
	}
	rep, err := probe.ScanMP4(writeTemp(t, "healthy.mp4", raw))
	if err != nil {
		t.Fatalf("ScanMP4: %v", err)
	}
	if rep.Format != probe.FormatMP4 || len(rep.Tracks) != 1 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	tk := rep.Tracks[0]
	if len(tk.Samples) != spec.SampleCount {
		t.Fatalf("samples=%d want %d", len(tk.Samples), spec.SampleCount)
	}
	if tk.Codec != "avc1" || !tk.Supported {
		t.Fatalf("codec=%s supported=%v", tk.Codec, tk.Supported)
	}
	if tk.TimeBase.Num != int64(spec.Timescale) {
		t.Fatalf("timescale=%d", tk.TimeBase.Num)
	}
	if tk.Width != 1920 || tk.Height != 1080 {
		t.Fatalf("dimensions=%dx%d", tk.Width, tk.Height)
	}
	// Deterministic duration: 1000 samples * 1001 ticks at 24000 Hz.
	wantDur := int64(spec.SampleCount) * int64(spec.FrameDelta) * 1_000_000_000 /
		int64(spec.Timescale)
	if got := tk.DurationNanos(); got != wantDur {
		t.Fatalf("duration=%d want %d", got, wantDur)
	}
	ks := rep.KeyStats[tk.TrackID]
	if ks.KeyframeCount != (spec.SampleCount+spec.KeyEvery-1)/spec.KeyEvery {
		t.Fatalf("keyframes=%d", ks.KeyframeCount)
	}
	if ks.P95MS <= 0 || ks.MaxMS < ks.P95MS {
		t.Fatalf("bad interval stats: %+v", ks)
	}
	// Sample offsets must point inside mdat and sizes match fixture.
	for i, s := range tk.Samples {
		if s.Size != int64(fixture.SampleSize(spec, i)) {
			t.Fatalf("sample %d size mismatch", i)
		}
		if s.Offset < 0 || s.Offset+s.Size > int64(len(raw)) {
			t.Fatalf("sample %d outside container", i)
		}
	}
}

func TestScanMP4Faults(t *testing.T) {
	base := fixture.DefaultSpec(41)

	t.Run("truncated_stsz", func(t *testing.T) {
		spec := base
		spec.TruncateSTSZ = true
		raw, _ := spec.BuildMP4()
		_, err := probe.ScanMP4(writeTemp(t, "trunc.mp4", raw))
		expectFault(t, err, fault.CatTruncatedSampleTable, 1)
	})
	t.Run("truncated_mdat", func(t *testing.T) {
		spec := base
		spec.TruncateMDAT = true
		raw, _ := spec.BuildMP4()
		_, err := probe.ScanMP4(writeTemp(t, "mdat.mp4", raw))
		expectFault(t, err, fault.CatTruncatedSampleTable, 1)
	})
	t.Run("illegal_crop", func(t *testing.T) {
		spec := base
		spec.IllegalCrop = true
		raw, _ := spec.BuildMP4()
		_, err := probe.ScanMP4(writeTemp(t, "crop.mp4", raw))
		expectFault(t, err, fault.CatIllegalCrop, 1)
	})
	t.Run("bad_time_base", func(t *testing.T) {
		spec := base
		spec.BadTimeBase = true
		raw, _ := spec.BuildMP4()
		_, err := probe.ScanMP4(writeTemp(t, "tb.mp4", raw))
		expectFault(t, err, fault.CatInvalidTimeBase, 1)
	})
}

func TestScanMXFHealthyAndFaults(t *testing.T) {
	spec := fixture.DefaultSpec(7)
	raw, err := spec.BuildMXF()
	if err != nil {
		t.Fatalf("BuildMXF: %v", err)
	}
	rep, err := probe.ScanMXF(writeTemp(t, "healthy.mxf", raw))
	if err != nil {
		t.Fatalf("ScanMXF: %v", err)
	}
	if rep.Format != probe.FormatMXF || len(rep.Tracks) != 1 {
		t.Fatalf("unexpected MXF report")
	}
	if len(rep.Tracks[0].Samples) != spec.SampleCount {
		t.Fatalf("MXF samples mismatch")
	}
	bad := spec
	bad.TruncateMDAT = true
	badRaw, _ := bad.BuildMXF()
	if _, err := probe.ScanMXF(writeTemp(t, "bad.mxf", badRaw)); err == nil {
		t.Fatal("expected truncated MXF to fail")
	}
}

func TestColourArbitration(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	spec.TrackColour = &[3]uint16{9, 16, 9} // BT.2020
	spec.ContainerColour = &[3]uint16{1, 1, 1}
	raw, _ := spec.BuildMP4()
	rep, err := probe.ScanMP4(writeTemp(t, "colr.mp4", raw))
	if err != nil {
		t.Fatalf("ScanMP4: %v", err)
	}
	dec := rep.Tracks[0].Colour
	if !dec.Conflict || dec.Source != probe.ScopeTrack ||
		dec.Adopted.Primaries != 9 {
		t.Fatalf("colour arbitration wrong: %+v", dec)
	}
	if len(rep.Conflicts) != 1 {
		t.Fatalf("conflicts=%d", len(rep.Conflicts))
	}

	// Container-only colour wins over default without conflict.
	spec2 := fixture.DefaultSpec(41)
	spec2.ContainerColour = &[3]uint16{1, 1, 1}
	raw2, _ := spec2.BuildMP4()
	rep2, err := probe.ScanMP4(writeTemp(t, "colr2.mp4", raw2))
	if err != nil {
		t.Fatalf("ScanMP4: %v", err)
	}
	if rep2.Tracks[0].Colour.Source != probe.ScopeContainer ||
		rep2.Tracks[0].Colour.Conflict {
		t.Fatalf("container scope wrong: %+v", rep2.Tracks[0].Colour)
	}
}

func TestRotationAndUnsupportedCodec(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	spec.Rotation = 90
	raw, _ := spec.BuildMP4()
	rep, err := probe.ScanMP4(writeTemp(t, "rot.mp4", raw))
	if err != nil {
		t.Fatalf("ScanMP4: %v", err)
	}
	if rep.Tracks[0].Rotation != 90 {
		t.Fatalf("rotation=%d", rep.Tracks[0].Rotation)
	}

	spec2 := fixture.DefaultSpec(41)
	spec2.Codec = "unknown" // fixture emits an unsupported fourCC
	raw2, _ := spec2.BuildMP4()
	rep2, err := probe.ScanMP4(writeTemp(t, "unk.mp4", raw2))
	if err != nil {
		t.Fatalf("unknown codec should not hard-fail: %v", err)
	}
	if rep2.Tracks[0].Supported {
		t.Fatal("unknown codec must be reported unsupported")
	}
	if rep2.Tracks[0].Codec != "mp4v" {
		t.Fatalf("unexpected codec fourCC: %s", rep2.Tracks[0].Codec)
	}
	if len(rep2.Faults) != 1 || rep2.Faults[0].Category != fault.CatUnsupportedCodec {
		t.Fatalf("expected unsupported codec fault, got %+v", rep2.Faults)
	}
}

func TestDeterministicReScan(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	raw, _ := spec.BuildMP4()
	p := writeTemp(t, "det.mp4", raw)
	r1, err := probe.ScanMP4(p)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := probe.ScanMP4(p)
	if err != nil {
		t.Fatal(err)
	}
	if r1.InputSHA256 != r2.InputSHA256 || r1.InputSHA256 == "" {
		t.Fatal("input digest not stable")
	}
	for i := range r1.Tracks {
		a, b := r1.Tracks[i].Samples, r2.Tracks[i].Samples
		if len(a) != len(b) {
			t.Fatal("sample count drift")
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("sample %d drifted across scans", j)
			}
		}
	}
}

func expectFault(t *testing.T, err error, cat fault.Category, track uint32) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected fault %s, got nil", cat)
	}
	fe := fault.AsError(err)
	if fe.Category != cat {
		t.Fatalf("category=%s want %s (err=%v)", fe.Category, cat, err)
	}
	if fe.TrackID != track {
		t.Fatalf("track=%d want %d", fe.TrackID, track)
	}
}
