package inspect

import (
	"bytes"
	"encoding/binary"
	"testing"

	"mediac/internal/core"
)

func gopFixture() []testSample {
	// 12 samples at 1000-tick (1 s) durations, keyframe every 4th sample.
	var samples []testSample
	for i := 0; i < 12; i++ {
		if i%4 == 0 {
			samples = append(samples, keyframeSample(1000))
		} else {
			samples = append(samples, deltaSample(1000))
		}
	}
	return samples
}

func TestInspectCleanFragment(t *testing.T) {
	data := buildFragment(1, 0, gopFixture())
	rec := core.NewRecorder()
	rep, err := InspectFragment("v0", 0, "avc1.640028", bytes.NewReader(data), rec)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Truncated || rep.HasZeroDur || rep.HasInvalidNAL {
		t.Fatalf("clean fragment flagged: %+v", rep)
	}
	if len(rep.Samples) != 12 {
		t.Fatalf("samples=%d", len(rep.Samples))
	}
	var keys int
	for _, s := range rep.Samples {
		if s.IsKeyframe {
			keys++
		}
	}
	if keys != 3 {
		t.Fatalf("keyframes=%d want 3", keys)
	}
	if len(rep.BoxBoundaries) != 2 ||
		rep.BoxBoundaries[0].Type != "moof" || rep.BoxBoundaries[1].Type != "mdat" {
		t.Fatalf("top-level box boundaries wrong: %+v", rep.BoxBoundaries)
	}
	if rec.HasDamaged() {
		t.Fatal("clean fragment must have no damage")
	}
}

func TestInspectTruncatedMdat(t *testing.T) {
	data := buildFragment(1, 0, gopFixture())
	// Locate the mdat header and cut the payload short while leaving its
	// declared size untouched, simulating a truncated media box.
	mdatIdx := bytes.Index(data, []byte("mdat"))
	declaredSize := binary.BigEndian.Uint32(data[mdatIdx-4 : mdatIdx])
	cut := int64(mdatIdx) + 4 + int64(declaredSize)/2 // keep header, half the body
	truncated := data[:cut]
	rec := core.NewRecorder()
	rep, err := InspectFragment("v0", 3, "avc1.640028", bytes.NewReader(truncated), rec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated {
		t.Fatal("truncated mdat must set the truncated flag")
	}
	kinds := map[core.FaultKind]bool{}
	for _, f := range rec.All() {
		kinds[f.Kind] = true
	}
	if !kinds[core.FaultTruncatedBox] {
		t.Fatalf("expected truncated_box fault, got %+v", kinds)
	}
}

func TestInspectZeroDuration(t *testing.T) {
	samples := withZeroDuration(gopFixture())
	data := buildFragment(1, 0, samples)
	rec := core.NewRecorder()
	rep, err := InspectFragment("v0", 1, "avc1.640028", bytes.NewReader(data), rec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasZeroDur {
		t.Fatal("zero duration must be detected")
	}
	var found bool
	for _, f := range rec.All() {
		if f.Kind == core.FaultZeroDuration && f.Source.SampleIndex == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("zero-duration sample fault missing")
	}
}

func TestInspectInvalidNAL(t *testing.T) {
	samples := gopFixture()
	// Corrupt the first sample's NAL length prefix to exceed the sample.
	data := buildFragment(1, 0, samples)
	mdatIdx := bytes.Index(data, []byte("mdat"))
	binary.BigEndian.PutUint32(data[mdatIdx+4:mdatIdx+8], 999999)
	rec := core.NewRecorder()
	rep, err := InspectFragment("v0", 2, "avc1.640028", bytes.NewReader(data), rec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasInvalidNAL {
		t.Fatal("oversized NAL length must be flagged invalid")
	}
}

func TestHEVCKeyframe(t *testing.T) {
	// HEVC IDR_W_RADL is type 19; header byte = type<<1.
	idrNAL := []byte{byte(hevcNALIDRWRadl << 1), 0xaa}
	trailNAL := []byte{1 << 1, 't', 'r'}
	samples := []testSample{
		sample(idrNAL, 2000),
		sample(trailNAL, 2000),
	}
	data := buildFragment(1, 0, samples)
	rec := core.NewRecorder()
	rep, err := InspectFragment("v0", 0, "hvc1.1.6.L120", bytes.NewReader(data), rec)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Samples[0].IsKeyframe || rep.Samples[1].IsKeyframe {
		t.Fatalf("HEVC keyframe classification wrong: %+v", rep.Samples)
	}
}

func TestGOPIntervals(t *testing.T) {
	rec := core.NewRecorder()
	var reports []*FragmentReport
	// Two fragments; keyframes at decode time 0 and 4000, then 8000.
	for frag, base := range []uint64{0, 4000} {
		data := buildFragment(1, base, gopFixture())
		rep, err := InspectFragment("v0", frag, "avc1", bytes.NewReader(data), rec)
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, rep)
	}
	gops := KeyframeGOPs(reports)
	// Each 12-sample fragment holds 3 keyframes, so two fragments yield 4
	// consecutive keyframe intervals.
	if len(gops) != 4 {
		t.Fatalf("gop intervals=%d want 4: %+v", len(gops), gops)
	}
	for _, g := range gops {
		if g.Ticks != 4000 {
			t.Fatalf("gop ticks=%d want 4000", g.Ticks)
		}
	}
}

func TestGOPAcrossClockReset(t *testing.T) {
	rec := core.NewRecorder()
	// Second fragment restarts its decode time at zero (discontinuity). The
	// backwards jump must not be reported as a negative GOP interval.
	r1, _ := InspectFragment("v0", 0, "avc1",
		bytes.NewReader(buildFragment(1, 9000, gopFixture()[:4])), rec)
	r2, _ := InspectFragment("v0", 1, "avc1",
		bytes.NewReader(buildFragment(1, 0, gopFixture()[:4])), rec)
	gops := KeyframeGOPs([]*FragmentReport{r1, r2})
	for _, g := range gops {
		if int64(g.Ticks) < 0 {
			t.Fatalf("negative GOP across clock reset: %+v", g)
		}
	}
}
