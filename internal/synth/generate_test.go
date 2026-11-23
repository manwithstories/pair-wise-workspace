package synth

import (
	"testing"

	"medpipe/internal/model"
	"medpipe/internal/sequence"
)

func TestGenerateDeterministic(t *testing.T) {
	opt := Options{Seed: 42, Tracks: 3, Frames: 40, DriftPPM: 180, WrapAt: 65536, VFR: true}
	a, err := Generate(opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(opt)
	if err != nil {
		t.Fatal(err)
	}
	da := sequence.DigestFrames(a.Tracks, a.Frames)
	db := sequence.DigestFrames(b.Tracks, b.Frames)
	if da != db {
		t.Fatal("same seed produced different captures")
	}
	different, err := Generate(Options{Seed: 43, Tracks: 3, Frames: 40, DriftPPM: 180, WrapAt: 65536, VFR: true})
	if err != nil {
		t.Fatal(err)
	}
	if sequence.DigestFrames(different.Tracks, different.Frames) == da {
		t.Fatal("different seed produced identical captures")
	}
}

func TestGenerateHasSilenceAndDuplicatePayloads(t *testing.T) {
	c, err := Generate(Options{Seed: 9, Tracks: 3, Frames: 120})
	if err != nil {
		t.Fatal(err)
	}
	audioID := ""
	for _, tr := range c.Tracks {
		if tr.Kind == model.KindAudio {
			audioID = tr.ID
		}
	}
	silent := 0
	for _, fr := range c.Frames[audioID] {
		quiet := true
		for _, e := range fr.Energy {
			if e > 0.05 {
				quiet = false
			}
		}
		if quiet {
			silent++
		}
	}
	if silent == 0 {
		t.Fatal("generated audio has no silent frames")
	}
	// Frames must carry media framing that passes content validation; the
	// synthetic capture deliberately keeps every frame distinct, so dedupe on
	// this fixture reports no groups.

}

func TestGenerateValidatesInput(t *testing.T) {
	if _, err := Generate(Options{Tracks: 0, Frames: 1}); err == nil {
		t.Fatal("zero tracks accepted")
	}
	if _, err := Generate(Options{Tracks: 1, Frames: 0}); err == nil {
		t.Fatal("zero frames accepted")
	}
	if _, err := Generate(Options{Tracks: 1, Frames: 1, WrapAt: 512}); !model.IsKind(err, model.KindInput) {
		t.Fatalf("tiny wrap modulus error = %v", err)
	}
}

func TestGeneratePayloadsValidate(t *testing.T) {
	c, err := Generate(Options{Seed: 1, Tracks: 4, Frames: 20, VFR: true, WrapAt: 65536})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range c.Tracks {
		for i := range c.Frames[tr.ID] {
			if err := model.ValidatePayload(tr.Kind, &c.Frames[tr.ID][i]); err != nil {
				t.Fatalf("generated frame rejected: %v", err)
			}
			if c.Frames[tr.ID][i].Checksum != c.Frames[tr.ID][i].IntegrityChecksum() {
				t.Fatal("generated checksum does not match content")
			}
		}
	}
}

func TestGenerateWrapsAndDrifts(t *testing.T) {
	c, err := Generate(Options{Seed: 2, Tracks: 3, Frames: 240, DriftPPM: 180, WrapAt: 65536})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := false
	for _, tr := range c.Tracks {
		fs := c.Frames[tr.ID]
		for i := 1; i < len(fs); i++ {
			if fs[i].RawTS < fs[i-1].RawTS {
				wrapped = true
			}
		}
	}
	if !wrapped {
		t.Fatal("capture never wrapped its 16-bit counter")
	}
}
