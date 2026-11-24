package render

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
)

func events() []model.Event {
	return []model.Event{
		{Kind: model.KindFrame, TrackID: 1, Start: 0, Duration: 90000, Sequence: 1, Keyframe: true},
		{Kind: model.KindSubtitle, TrackID: 2, Start: 0, Duration: 45000, Payload: []byte("hello")},
		{Kind: model.KindFrame, TrackID: 1, Start: 90000, Duration: 90000, Sequence: 2, Keyframe: true},
		{Kind: model.KindEmsg, Start: 90000, Duration: 1000, Payload: []byte("cue")},
		{Kind: model.KindFrame, TrackID: 1, Start: 180000, Duration: 90000, Sequence: 3, Keyframe: true},
	}
}

func TestRenderDeterministic(t *testing.T) {
	seq1, err := Render(events(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := Render(events(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(seq1.Frames) != 3 {
		t.Fatalf("frames %d", len(seq1.Frames))
	}
	for i := range seq1.Frames {
		if seq1.Frames[i].SHA256 != seq2.Frames[i].SHA256 {
			t.Fatalf("frame %d not deterministic", i)
		}
		if seq1.Frames[i].Start != seq2.Frames[i].Start {
			t.Fatalf("frame %d timing", i)
		}
		img, err := png.Decode(bytes.NewReader(seq1.Frames[i].PNG))
		if err != nil {
			t.Fatalf("frame %d png: %v", i, err)
		}
		if img.Bounds().Dx() != seq1.Width || img.Bounds().Dy() != seq1.Height {
			t.Fatalf("frame %d size %v", i, img.Bounds())
		}
	}
	if !bytes.Equal(seq1.Manifest, seq2.Manifest) {
		t.Fatal("manifest not deterministic")
	}
	// The manifest contains one sha per frame.
	if n := bytes.Count(seq1.Manifest, []byte(`"sha256"`)); n != len(seq1.Frames) {
		t.Fatalf("manifest entries %d", n)
	}
}

func TestRenderIncludesMetadataPoints(t *testing.T) {
	evs := []model.Event{
		{Kind: model.KindSubtitle, TrackID: 2, Start: 5000, Duration: 100},
		{Kind: model.KindChapter, Start: 7000},
	}
	seq, err := Render(evs, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(seq.Frames) != 2 {
		t.Fatalf("frames %d", len(seq.Frames))
	}
	if seq.Frames[0].Start != 5000 || seq.Frames[1].Start != 7000 {
		t.Fatalf("order: %d %d", seq.Frames[0].Start, seq.Frames[1].Start)
	}
}

func TestRenderLimits(t *testing.T) {
	if _, err := Render(nil, DefaultConfig()); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty events: %v", err)
	}
	if _, err := Render(events(), Config{Width: 4, Height: 4}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("tiny raster: %v", err)
	}
	if _, err := Render(events(), Config{Width: MaxWidth + 1, Height: 90}); !errors.Is(err, model.ErrLimit) {
		t.Fatalf("large raster: %v", err)
	}
	// The frame cap keeps memory bounded.
	var many []model.Event
	for i := 0; i < MaxFrames+50; i++ {
		many = append(many, model.Event{Kind: model.KindFrame, TrackID: 1, Start: clock.Tick(i * 1000)})
	}
	seq, err := Render(many, Config{Width: 16, Height: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(seq.Frames) != MaxFrames {
		t.Fatalf("capped frames %d", len(seq.Frames))
	}
}

func TestWriteTo(t *testing.T) {
	seq, err := Render(events(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := seq.WriteTo(dir); err != nil {
		t.Fatal(err)
	}
	for i := range seq.Frames {
		name := filepath.Join(dir, "frames", formatFrameName(i+1))
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(data) == 0 {
			t.Fatalf("frame %d empty", i)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.jsonl")); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	empty := &Sequence{}
	if err := empty.WriteTo(dir); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty sequence: %v", err)
	}
}

func formatFrameName(n int) string {
	return "frame-" + pad3(n) + ".png"
}

func pad3(n int) string {
	s := []byte("000")
	for i := 2; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}
