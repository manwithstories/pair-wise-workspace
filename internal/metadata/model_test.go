package metadata

import (
	"errors"
	"testing"

	"mediamerge/internal/clock"
	"mediamerge/internal/fault"
)

func u16(v uint16) *uint16 { return &v }
func bl(v bool) *bool      { return &v }

func TestResolveColorPrecedence(t *testing.T) {
	// Container says BT.709, track says BT.2020 primaries but agrees on the
	// rest, explicit pins the matrix. The track value wins over the
	// container; the conflict is reported but not fatal.
	container := ColorDesc{Primaries: u16(1), Transfer: u16(1), Matrix: u16(1), FullRange: bl(false), Source: "container"}
	track := ColorDesc{Primaries: u16(9), Transfer: u16(1), Source: "track"}
	explicit := ColorDesc{Matrix: u16(9), Source: "explicit"}

	var report fault.Report
	got, err := ResolveColor(&report, 3, &container, &track, &explicit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Primaries != 9 || got.Matrix != 9 || got.Transfer != 1 {
		t.Fatalf("bad precedence %+v", got)
	}
	if got.FullRange {
		t.Fatal("range should be limited")
	}
	if got.Sources["primaries"] != "track" || got.Sources["matrix"] != "explicit" || got.Sources["transfer"] != "track" || got.Sources["range"] != "container" {
		t.Fatalf("bad sources %+v", got.Sources)
	}
	if len(report.Items) == 0 {
		t.Fatal("conflict must be reported")
	}
	var found bool
	for _, d := range report.Items {
		if d.Code == fault.CodeColorConflict && d.Field == "primaries" {
			found = true
			if d.Severity != fault.SeverityWarn || len(d.Rejected) != 1 || d.TrackID != 3 {
				t.Fatalf("bad diagnostic %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("primaries conflict not reported")
	}
	if got.Canonical() == "" {
		t.Fatal("canonical empty")
	}
}

func TestResolveColorMissingIsFatal(t *testing.T) {
	// Nothing supplies transfer, so delivery must fail with a locatable
	// error that names the track and the field.
	container := ColorDesc{Primaries: u16(1), Matrix: u16(1), FullRange: bl(true)}
	var report fault.Report
	_, err := ResolveColor(&report, 4, &container, nil, nil)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeColorMissing || fe.TrackID != 4 {
		t.Fatalf("bad error %v", err)
	}
	if !report.HasErrors() {
		t.Fatal("error must be present in report")
	}
}

func TestResolveColorUnspecifiedIsMissing(t *testing.T) {
	// CICP value 2 means "unspecified" and must not count as a description.
	c := NewColorDesc(2, 2, 2, nil, "container")
	if !c.Empty() {
		t.Fatalf("unspecified should be empty: %+v", c)
	}
	var report fault.Report
	if _, err := ResolveColor(&report, 1, &c, nil, nil); err == nil {
		t.Fatal("expected missing colour error")
	}
	if len(report.Errors()) != 4 {
		t.Fatalf("want 4 missing fields got %d", len(report.Errors()))
	}
}

func TestColorConflictBetweenTrackAndExplicit(t *testing.T) {
	track := ColorDesc{Primaries: u16(1), Transfer: u16(1), Matrix: u16(1), FullRange: bl(false)}
	explicit := ColorDesc{FullRange: bl(true), Transfer: u16(16)}
	var report fault.Report
	got, err := ResolveColor(&report, 2, nil, &track, &explicit)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FullRange || got.Transfer != 16 {
		t.Fatalf("explicit should beat track: %+v", got)
	}
	if len(report.Errors()) != 0 || len(report.Items) != 2 {
		t.Fatalf("want 2 warnings, got %v", report.Items)
	}
	for _, d := range report.Items {
		if d.Code != fault.CodeColorConflict {
			t.Fatalf("unexpected diagnostic %+v", d)
		}
	}
}

func TestCropValidation(t *testing.T) {
	tr := &Track{ID: 8, Kind: KindVideo, Timebase: clock.Must(25, 1), DurationTicks: 50, Width: 1920, Height: 1080}
	ok := Crop{X: 0, Y: 0, Width: 1440, Height: 1080}
	if err := ok.Validate(tr); err != nil {
		t.Fatal(err)
	}
	bad := []Crop{
		{X: 1000, Y: 0, Width: 1000, Height: 100}, // right edge 2000 > 1920
		{X: -1, Y: 0, Width: 10, Height: 10},
		{X: 0, Y: 0, Width: 0, Height: 10},
	}
	for _, c := range bad {
		err := c.Validate(tr)
		var fe *fault.Error
		if !errors.As(err, &fe) || fe.Code != fault.CodeCropOutOfBounds || fe.TrackID != 8 || fe.Span == nil {
			t.Fatalf("crop %v: bad error %v", c, err)
		}
		if fe.Span.End != clock.MustRational(2, 1) {
			t.Fatalf("span %v", fe.Span)
		}
	}
}

func TestRotationNormalization(t *testing.T) {
	span := clock.Span{Start: clock.MustRational(0, 1), End: clock.MustRational(1, 1)}
	for _, c := range []struct {
		in   float64
		want int
	}{{0, 0}, {90, 90}, {180, 180}, {270, 270}, {-90, 270}, {360, 0}, {450.0001, 90}} {
		got, err := NormalizeRotation(1, c.in, span)
		if err != nil || got != c.want {
			t.Fatalf("%v -> %d err %v want %d", c.in, got, err, c.want)
		}
	}
	if _, err := NormalizeRotation(5, 45, span); err == nil {
		t.Fatal("45 degrees must be rejected")
	} else {
		var fe *fault.Error
		if !errors.As(err, &fe) || fe.Code != fault.CodeRotationInvalid || fe.TrackID != 5 {
			t.Fatalf("bad error %v", err)
		}
	}
}

func TestKeyframeIndexHit(t *testing.T) {
	tb := clock.Must(25, 1)
	deltas := make([]int64, 40)
	for i := range deltas {
		deltas[i] = 1
	}
	idx, err := BuildKeyframeIndex(deltas, []int{1, 10, 20, 30}, tb)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Empty() {
		t.Fatal("empty index")
	}
	kf, ok := idx.Hit(0)
	if !ok || kf.Sample != 1 {
		t.Fatalf("hit at 0: %+v %v", kf, ok)
	}
	kf, ok = idx.Hit(19)
	if !ok || kf.Sample != 20 {
		t.Fatalf("hit at 19: %+v %v", kf, ok)
	}
	kf, ok = idx.Hit(9)
	if !ok || kf.Sample != 10 {
		t.Fatalf("hit at 9: %+v %v", kf, ok)
	}
	if _, ok := idx.Hit(-1); ok {
		t.Fatal("negative tick must miss")
	}
	// Random access must agree with a linear scan for every tick.
	for tick := int64(-2); tick <= 45; tick++ {
		var want Keyframe
		found := false
		for _, e := range idx.Entries() {
			if e.Tick <= tick {
				want, found = e, true
			}
		}
		got, ok := idx.Hit(tick)
		if ok != found || (ok && got != want) {
			t.Fatalf("tick %d: got %+v %v want %+v %v", tick, got, ok, want, found)
		}
	}
}

func TestKeyframeIndexRejectsBadInput(t *testing.T) {
	tb := clock.Must(25, 1)
	if _, err := BuildKeyframeIndex(nil, []int{1}, tb); err == nil {
		t.Fatal("empty deltas")
	}
	if _, err := BuildKeyframeIndex([]int64{1, 0}, []int{1}, tb); err == nil {
		t.Fatal("zero delta")
	}
	if _, err := BuildKeyframeIndex([]int64{1, 1}, []int{1, 3}, tb); err == nil {
		t.Fatal("out of range key")
	}
	if _, err := BuildKeyframeIndex([]int64{1, 1}, []int{1, 1}, tb); err == nil {
		t.Fatal("duplicate key")
	}
}

func TestKeyframeHitSpan(t *testing.T) {
	tb := clock.Must(25, 1)
	deltas := []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	idx, err := BuildKeyframeIndex(deltas, []int{1, 6}, tb)
	if err != nil {
		t.Fatal(err)
	}
	span := tb.Span(4, 10) // ticks 4..14 -> seconds 4/25..14/25
	kf, ok := idx.HitSpan(span, tb)
	if !ok || kf.Sample != 1 {
		t.Fatalf("span hit %+v %v", kf, ok)
	}
	span = tb.Span(6, 2)
	if kf, ok = idx.HitSpan(span, tb); !ok || kf.Sample != 6 {
		t.Fatalf("span 2 hit %+v %v", kf, ok)
	}
}

func TestTrackCanonicalStable(t *testing.T) {
	var report fault.Report
	tr := &Track{
		ID: 1, Kind: KindVideo, Timebase: clock.Must(24000, 1001), DurationTicks: 24000,
		Width: 3840, Height: 2160, Rotation: 90,
		Crop:  &Crop{X: 0, Y: 0, Width: 1920, Height: 1080},
		Color: ColorDesc{Primaries: u16(9), Transfer: u16(16), Matrix: u16(9), FullRange: bl(false)},
	}
	if _, err := tr.Normalize(&report, nil, nil); err != nil {
		t.Fatal(err)
	}
	want := "track=1 kind=video tb=24000/1001 Hz dur=24000 size=3840x2160 rot=90 crop=1920x1080+0+0 resolved=p=9:bt2020,t=16:smpte2084,m=9:bt2020nc,r=limited keys=0"
	if got := tr.Canonical(); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if tr.Canonical() != want {
		t.Fatal("canonical not deterministic")
	}
}
