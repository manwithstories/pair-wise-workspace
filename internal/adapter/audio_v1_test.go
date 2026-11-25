package adapter

import (
	"bytes"
	"testing"

	"avpipe/internal/media"
)

// TestAudioSectionAlignment verifies that audio section headers and PCM
// descriptors are read at the correct absolute offsets.
func TestAudioSectionAlignment(t *testing.T) {
	cfg := DefaultGenConfig(5, 7)
	raw, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseContainer(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Audio) != 5 {
		t.Fatalf("audio blocks=%d, want 5", len(res.Audio))
	}
	want := int(cfg.AudioRate) / 30
	want = (want / 2) * 2 // stereo-aligned
	for _, ah := range res.Audio {
		if len(ah.Samples) != want {
			t.Fatalf("seq %d samples=%d, want %d", ah.Sequence, len(ah.Samples), want)
		}
		if ah.SampleRate != cfg.AudioRate || ah.Layout != media.LayoutStereo {
			t.Fatalf("unexpected block header: rate=%d layout=%s", ah.SampleRate, ah.Layout)
		}
	}
}
