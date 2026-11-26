package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"strings"
	"testing"

	"pairflow/internal/media"
	"pairflow/internal/timeline"
)

func fixture(t *testing.T) (*timeline.RawTimeline, []timeline.Resolved) {
	t.Helper()
	tl, err := timeline.Generate(17, 3)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	res, err := timeline.Resolve(tl)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return tl, res
}

func videoTracks(t *testing.T, res []timeline.Resolved) []VideoTrack {
	t.Helper()
	vt, err := PrepareVideo(res, map[string]int{"cam-a": 1, "cam-b": 0})
	if err != nil {
		t.Fatalf("prepare video: %v", err)
	}
	return vt
}

func TestPrepareVideoRejectsUnknownColor(t *testing.T) {
	_, res := fixture(t)
	// Corrupt one clip's declared space.
	for i := range res {
		if res[i].TrackID == "cam-a" {
			res[i].Segments[0].Video = &timeline.VideoSpec{Space: "p3", Pattern: "bars", Tint: "teal"}
		}
	}
	_, err := PrepareVideo(res, nil)
	if err == nil || err.Class != media.ClassInput {
		t.Fatalf("unknown color space must be class-2, got %v", err)
	}
}

func TestPrepareVideoRejectsUnknownPattern(t *testing.T) {
	_, res := fixture(t)
	for i := range res {
		if res[i].TrackID == "cam-b" {
			res[i].Segments[0].Video = &timeline.VideoSpec{Space: "srgb", Pattern: "noise", Tint: "teal"}
		}
	}
	_, err := PrepareVideo(res, nil)
	if err == nil || err.Class != media.ClassInput {
		t.Fatalf("unknown pattern must be class-2, got %v", err)
	}
}

func TestRenderFrameCount(t *testing.T) {
	tl, res := fixture(t)
	vt := videoTracks(t, res)
	d, _ := tl.Duration.Media()
	frames, err := FrameCount(d)
	if err != nil || frames != 72 {
		t.Fatalf("3 s @24 fps = 72 frames, got %d (%v)", frames, err)
	}
	for idx := 0; idx < frames; idx++ {
		f, err := RenderFrame(vt, idx)
		if err != nil {
			t.Fatalf("frame %d: %v", idx, err)
		}
		if f.Image.Bounds() != image.Rect(0, 0, 320, 180) {
			t.Fatalf("frame %d bounds %v", idx, f.Image.Bounds())
		}
		if len(f.Layers) == 0 {
			t.Fatalf("frame %d has no layers", idx)
		}
	}
}

func TestFramesDifferByHash(t *testing.T) {
	_, res := fixture(t)
	vt := videoTracks(t, res)
	seen := map[string]int{}
	for idx := 0; idx < 72; idx++ {
		f, err := RenderFrame(vt, idx)
		if err != nil {
			t.Fatalf("frame %d: %v", idx, err)
		}
		h := hashImage(f.Image)
		if prev, dup := seen[h]; dup {
			t.Fatalf("frame %d duplicates frame %d", idx, prev)
		}
		seen[h] = idx
	}
}

func TestDissolveContinuity(t *testing.T) {
	_, res := fixture(t)
	// Isolate cam-a so the dissolve weights are not mixed with the cross-track
	// composite weight.
	var camA []timeline.Resolved
	for _, tr := range res {
		if tr.TrackID == "cam-a" {
			camA = append(camA, tr)
		}
	}
	vt, err := PrepareVideo(camA, map[string]int{"cam-a": 0})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// cam-a dissolves a1->a2 for 0.40 s ending at clip a2's start (0.95 s).
	// Rendering every frame must yield one continuous monotone window.
	var weights []float64
	var layers []string
	for idx := 0; idx < 72; idx++ {
		f, err := RenderFrame(vt, idx)
		if err != nil {
			t.Fatalf("frame %d: %v", idx, err)
		}
		if len(f.Layers) == 2 {
			if layers == nil {
				layers = f.Layers
			}
			weights = append(weights, f.Blend)
		}
	}
	// cam-a dissolves twice (a1->a2 0.40 s, a2->a3 0.25 s). Each is a
	// contiguous window whose weights rise monotonically from 0; a reset
	// marks the start of the next window. Every frame in between is rendered.
	if len(weights) < 5 {
		t.Fatalf("cam-a dissolve not observable: n=%d", len(weights))
	}
	windows := 0
	prev := -1.0
	for i, w := range weights {
		if w == 0 || w < prev {
			windows++
			prev = -1
		}
		if w <= prev {
			t.Fatalf("dissolve weight not monotonic at %d: %v", i, weights)
		}
		prev = w
	}
	if windows != 2 {
		t.Fatalf("want 2 dissolve windows for cam-a, got %d: %v", windows, weights)
	}
	if layers == nil || layers[0] == layers[1] {
		t.Fatalf("dissolve layers are identical: %v", layers)
	}
}

func TestTimelineContinuityAcrossBoundary(t *testing.T) {
	_, res := fixture(t)
	vt := videoTracks(t, res)
	// Every frame must be rendered; no gap at a cut or dissolve boundary.
	for idx := 0; idx < 72; idx++ {
		if _, err := RenderFrame(vt, idx); err != nil {
			t.Fatalf("frame %d missing: %v", idx, err)
		}
	}
}

func TestRenderPCMCounts(t *testing.T) {
	tl, res := fixture(t)
	at, _, err := PrepareAudio(res)
	if err != nil {
		t.Fatalf("prepare audio: %v", err)
	}
	d, _ := tl.Duration.Media()
	samples, err := SampleCount(d)
	if err != nil || samples != 144000 {
		t.Fatalf("samples %d (%v)", samples, err)
	}
	pcm, err := RenderPCM(at, samples, 1024)
	if err != nil {
		t.Fatalf("render pcm: %v", err)
	}
	if int64(len(pcm)) != samples*media.AudioChannels {
		t.Fatalf("pcm length %d, want %d", len(pcm), samples*media.AudioChannels)
	}
	// Six channels must carry signal, including the LFE and surrounds.
	for ch := 0; ch < 6; ch++ {
		var peak int
		for i := ch; i < len(pcm); i += 6 {
			v := int(pcm[i])
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak == 0 {
			t.Fatalf("channel %d is silent", ch)
		}
	}
}

func TestPrepareAudioRejectsUnknownRate(t *testing.T) {
	_, res := fixture(t)
	for i := range res {
		if res[i].TrackID == "boom" {
			res[i].Segments[0].Audio = &timeline.AudioSpec{Rate: 22050, Channels: 1, Label: "bad"}
		}
	}
	if _, _, err := PrepareAudio(res); err == nil || err.Class != media.ClassInput {
		t.Fatal("unknown sample rate must be rejected")
	}
}

// hashImage hashes the frame pixels in a stable scan order.
func hashImage(img *image.NRGBA) string {
	var b strings.Builder
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			c := img.NRGBAAt(x, y)
			b.WriteString(string([]byte{c.R, c.G, c.B, c.A}))
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
