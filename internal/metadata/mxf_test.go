package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

func mxfDoc(t *testing.T) *MXFDocument {
	t.Helper()
	ts := make([]int64, 12)
	for i := range ts {
		ts[i] = int64(i) * 1000
	}
	keys := []int{0, 4, 8}
	return &MXFDocument{
		EditUnitBytes:  256,
		EditUnits:      12,
		IndexInterval:  1,
		ContainerColor: &ColorDesc{Primaries: u16(1), Transfer: u16(1), Matrix: u16(1), FullRange: bl(false)},
		Tracks: []MXFTrack{
			{
				ID: 1, Kind: KindVideo, Timebase: clock.Must(24000, 1000), Duration: 12000,
				Width: 1920, Height: 1080, StoredRot: 180,
				Crop:           &Crop{X: 0, Y: 0, Width: 1920, Height: 800},
				Color:          ColorDesc{Primaries: u16(9), Transfer: u16(16), Matrix: u16(9), FullRange: bl(false)},
				TimestampTicks: ts, KeyEditUnits: keys,
			},
			{
				ID: 2, Kind: KindAudio, Timebase: clock.Must(48000, 1), Duration: 24000,
				SampleRate: 48000, Channels: 6,
				TimestampTicks: ts, KeyEditUnits: keys,
			},
		},
	}
}

func writeMXFTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.mxf")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMXFRoundtrip(t *testing.T) {
	doc := mxfDoc(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseMXF(writeMXFTemp(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if f.Container != "mxf" || len(f.Tracks) != 2 {
		t.Fatalf("bad file %+v", f)
	}
	v := f.Tracks[0]
	if v.Width != 1920 || v.Height != 1080 || v.Rotation != 180 {
		t.Fatalf("video %+v", v)
	}
	if v.Crop == nil || v.Crop.Width != 1920 || v.Crop.Height != 800 {
		t.Fatalf("crop %+v", v.Crop)
	}
	if v.Color.Primaries == nil || *v.Color.Primaries != 9 {
		t.Fatalf("colour %+v", v.Color)
	}
	keys := v.Keyframes.Entries()
	if len(keys) != 3 || keys[0].Sample != 1 || keys[1].Sample != 5 {
		t.Fatalf("keys %+v", keys)
	}
	if f.Color == nil || f.Color.Transfer == nil || *f.Color.Transfer != 1 {
		t.Fatalf("container colour %+v", f.Color)
	}
	a := f.Tracks[1]
	if a.SampleRate != 48000 || a.Channels != 6 {
		t.Fatalf("audio %+v", a)
	}
}

func TestMXFTruncatedPartitionIsLocated(t *testing.T) {
	doc := mxfDoc(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	// Cut inside the body so a KLV declares more bytes than remain.
	path := writeMXFTemp(t, data[:len(data)/2])
	_, err = ParseMXF(path)
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want fault got %v", err)
	}
	if fe.Code != fault.CodeTruncatedPartition || fe.Stage != fault.StageParse {
		t.Fatalf("bad fault %+v", fe)
	}
	if !contains(fe.Detail, "declares") || !contains(fe.Detail, "ends at") {
		t.Fatalf("detail lacks location: %q", fe.Detail)
	}
}

func TestMXFTruncatedHeaderIsLocated(t *testing.T) {
	doc := mxfDoc(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	path := writeMXFTemp(t, data[:12])
	_, err = ParseMXF(path)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeTruncatedPartition {
		t.Fatalf("bad error %v", err)
	}
	if !contains(fe.Detail, "KLV header") {
		t.Fatalf("detail %q", fe.Detail)
	}
}

func TestMXFNonMonotonicTimestamps(t *testing.T) {
	doc := mxfDoc(t)
	doc.Tracks[0].TimestampTicks[5] = doc.Tracks[0].TimestampTicks[4]
	doc.Tracks[0].TimestampTicks[6] = 10
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseMXF(writeMXFTemp(t, data))
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeNonMonotonic {
		t.Fatalf("bad error %v", err)
	}
	if fe.TrackID != 1 || fe.Span == nil {
		t.Fatalf("missing location %+v", fe)
	}
}

func TestMXFOddRotationRejectedByResolve(t *testing.T) {
	doc := mxfDoc(t)
	doc.Tracks[0].StoredRot = 45
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseMXF(writeMXFTemp(t, data))
	if err != nil {
		t.Fatal(err)
	}
	var report fault.Report
	_, err = f.Tracks[0].Normalize(&report, f.Color, nil)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeRotationInvalid || fe.TrackID != 1 {
		t.Fatalf("bad error %v", err)
	}
}

func TestMXFContainerColorFillsGap(t *testing.T) {
	doc := mxfDoc(t)
	// Track omits transfer; the container supplies it, which must resolve
	// without a silent overwrite.
	doc.Tracks[0].Color.Transfer = nil
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseMXF(writeMXFTemp(t, data))
	if err != nil {
		t.Fatal(err)
	}
	var report fault.Report
	got, err := f.Tracks[0].Normalize(&report, f.Color, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transfer != 1 || got.Sources["transfer"] != "container" {
		t.Fatalf("transfer not filled: %+v", got)
	}
	if got.Primaries != 9 || got.Sources["primaries"] != "track" {
		t.Fatalf("track colour must win: %+v", got)
	}
	if len(report.Errors()) != 0 {
		t.Fatalf("unexpected errors %v", report.Items)
	}
}

func TestMXFDeterministic(t *testing.T) {
	doc := mxfDoc(t)
	a, _ := doc.Write()
	b, _ := doc.Write()
	if string(a) != string(b) {
		t.Fatal("mxf output not deterministic")
	}
}
