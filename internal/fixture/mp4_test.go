package fixture

import (
	"bytes"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

func samplePreviewSpec() PreviewSpec {
	frames := make([]PreviewFrame, 30)
	for i := range frames {
		frames[i] = PreviewFrame{
			Sample:     uint32(i + 1),
			TimeScaled: uint32(i) * 1000,
			Keyframe:   i%10 == 0,
		}
	}
	return PreviewSpec{Timescale: 30000, Delta: 1000, Frames: frames}
}

func TestPreviewMP4RoundTripIndex(t *testing.T) {
	spec := samplePreviewSpec()
	raw, err := BuildPreviewMP4(spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(raw) < 8 {
		t.Fatal("output too short to be an MP4 box stream")
	}
	// Deterministic build.
	again, err := BuildPreviewMP4(spec)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("preview MP4 build is not deterministic")
	}

	var (
		gotStts *mp4.Stts
		gotStss *mp4.Stss
		gotMvhd *mp4.Mvhd
	)
	_, err = mp4.ReadBoxStructure(bytes.NewReader(raw), func(h *mp4.ReadHandle) (any, error) {
		box, _, perr := h.ReadPayload()
		if perr != nil {
			return nil, perr
		}
		switch b := box.(type) {
		case *mp4.Mvhd:
			gotMvhd = b
		case *mp4.Stts:
			gotStts = b
		case *mp4.Stss:
			gotStss = b
		}
		// Recurse into container boxes so leaves are visited.
		if _, err := h.Expand(); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("read structure: %v", err)
	}
	if gotMvhd == nil || gotMvhd.Timescale != 30000 {
		t.Fatalf("mvhd timescale missing: %+v", gotMvhd)
	}
	if gotStts == nil || len(gotStts.Entries) != 1 ||
		gotStts.Entries[0].SampleCount != 30 || gotStts.Entries[0].SampleDelta != 1000 {
		t.Fatalf("stts time index mismatch: %+v", gotStts)
	}
	if gotStss == nil || len(gotStss.SampleNumber) != 3 {
		t.Fatalf("stss keyframe table mismatch: %+v", gotStss)
	}
	wantKeys := []uint32{1, 11, 21}
	for i, want := range wantKeys {
		if gotStss.SampleNumber[i] != want {
			t.Fatalf("stss[%d] = %d, want %d", i, gotStss.SampleNumber[i], want)
		}
	}
}

func TestPreviewMP4RejectsInvalidSpec(t *testing.T) {
	if _, err := BuildPreviewMP4(PreviewSpec{}); err == nil {
		t.Fatal("empty preview spec must be rejected")
	}
	if _, err := BuildPreviewMP4(PreviewSpec{Timescale: 1000, Delta: 1}); err == nil {
		t.Fatal("frame-less preview spec must be rejected")
	}
}
