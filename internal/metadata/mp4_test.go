package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

func testDocument(t *testing.T) *MP4Document {
	t.Helper()
	doc, err := BuildDocument(WriteOptions{
		ContainerColor: ColorDesc{Primaries: u16(1), Transfer: u16(1), Matrix: u16(1), FullRange: bl(false)},
		Tracks: []WriteTrack{
			{
				Kind: KindVideo, Timescale: 24000, Duration: 24000, SampleDelta: 1000, SampleCount: 24,
				Width: 1920, Height: 1080,
				Matrix: MatrixForRotation(90),
				Crop:   &Crop{X: 0, Y: 0, Width: 1440, Height: 1080},
				Color:  ColorDesc{Primaries: u16(9), Transfer: u16(16), Matrix: u16(9), FullRange: bl(false)},
			},
			{
				Kind: KindAudio, Timescale: 48000, Duration: 48000, SampleDelta: 1000, SampleCount: 48,
				SampleRate: 48000, Channels: 2,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Tracks[0].KeySamples = []uint32{1, 9, 17}
	return doc
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMP4Roundtrip(t *testing.T) {
	doc := testDocument(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseMP4(writeTemp(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("got %d tracks", len(f.Tracks))
	}
	v := f.Tracks[0]
	if v.Kind != KindVideo || v.ID != 1 || v.Width != 1920 || v.Height != 1080 {
		t.Fatalf("bad video track %+v", v)
	}
	if v.Rotation != 90 {
		t.Fatalf("rotation %d want 90", v.Rotation)
	}
	if v.Crop == nil || *v.Crop != (Crop{X: 0, Y: 0, Width: 1440, Height: 1080}) {
		t.Fatalf("crop %+v", v.Crop)
	}
	if v.Color.Primaries == nil || *v.Color.Primaries != 9 || v.Color.Transfer == nil || *v.Color.Transfer != 16 {
		t.Fatalf("colour %s", v.Color.Canonical())
	}
	if v.Timebase != clock.Must(24000, 1) || v.DurationTicks != 24000 {
		t.Fatalf("timing %v %d", v.Timebase, v.DurationTicks)
	}
	if len(v.Keyframes.Entries()) != 3 {
		t.Fatalf("keys %+v", v.Keyframes.Entries())
	}
	kf, ok := v.Keyframes.Hit(8000)
	if !ok || kf.Sample != 9 {
		t.Fatalf("keyframe hit %+v %v", kf, ok)
	}
	a := f.Tracks[1]
	if a.Kind != KindAudio || a.SampleRate != 48000 || a.Channels != 2 {
		t.Fatalf("bad audio track %+v", a)
	}
	if f.Color == nil || f.Color.Primaries == nil || *f.Color.Primaries != 1 {
		t.Fatalf("container colour %+v", f.Color)
	}
}

func TestMP4WriteDeterministic(t *testing.T) {
	doc := testDocument(t)
	a, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	b, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("writer output is not deterministic")
	}
}

func TestMP4TruncatedBoxIsLocated(t *testing.T) {
	doc := testDocument(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	// Inflate the moov size field beyond EOF: locate moov's header.
	idx := indexOf(data, []byte("moov"))
	if idx < 4 {
		t.Fatal("moov not found")
	}
	data[idx-4] = 0x7f
	path := writeTemp(t, data)
	_, err = ParseMP4(path)
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want fault, got %v", err)
	}
	if fe.Code != fault.CodeTruncatedBox || fe.Stage != fault.StageParse {
		t.Fatalf("bad fault %+v", fe)
	}
	if want := "offset " + itoa(int64(idx-4)); !contains(fe.Detail, want) {
		t.Fatalf("detail %q lacks %q", fe.Detail, want)
	}
}

func TestMP4TruncatedFileIsLocated(t *testing.T) {
	doc := testDocument(t)
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, data[:len(data)-40])
	_, err = ParseMP4(path)
	if err == nil {
		t.Fatal("expected error")
	}
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("want fault, got %v", err)
	}
	if fe.Code != fault.CodeTruncatedBox {
		t.Fatalf("code %s", fe.Code)
	}
}

func TestMP4BadClapIsRejected(t *testing.T) {
	doc := testDocument(t)
	// Crop wider than the coded picture.
	doc.Tracks[0].Track.Crop = &Crop{X: 0, Y: 0, Width: 2000, Height: 1080}
	data, err := doc.Write()
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseMP4(writeTemp(t, data))
	if err != nil {
		t.Fatal(err)
	}
	var report fault.Report
	if _, err := f.Tracks[0].Normalize(&report, f.Color, nil); err == nil {
		t.Fatal("expected crop error")
	} else {
		var fe *fault.Error
		if !errors.As(err, &fe) || fe.Code != fault.CodeCropOutOfBounds || fe.TrackID != 1 {
			t.Fatalf("bad error %v", err)
		}
	}
}

func TestRotationForMatrixRejectsArbitraryMatrix(t *testing.T) {
	span := clock.Span{Start: clock.MustRational(0, 1), End: clock.MustRational(1, 1)}
	odd := [9]int32{46341, 46341, 0, -46341, 46341, 0, 0, 0, 1 << 30}
	_, err := RotationForMatrix(9, odd, span)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeRotationInvalid || fe.TrackID != 9 {
		t.Fatalf("bad error %v", err)
	}
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
