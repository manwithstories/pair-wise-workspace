package testkit

import (
	"bytes"
	"errors"
	"testing"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

func TestGenerateMediaDeterministic(t *testing.T) {
	a, err := GenerateMedia(42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateMedia(42)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes, b.Bytes) {
		t.Fatal("media is not deterministic for a given seed")
	}
	c, err := GenerateMedia(43)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes, c.Bytes) {
		t.Fatal("different seeds produced identical media")
	}
	// The media must round-trip through the real parser.
	m, err := parse.ParseMovie(a.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Tracks) != 5 {
		t.Fatalf("tracks %d", len(m.Tracks))
	}
	again, err := parse.EncodeMovie(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, a.Bytes) {
		t.Fatal("generated media does not round-trip byte-for-byte")
	}
}

func TestGeneratedEventsCoverAllKinds(t *testing.T) {
	media, err := GenerateMedia(7)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[model.Kind]int{}
	for _, ev := range media.ExpectedEvents {
		kinds[ev.Kind]++
	}
	for _, k := range []model.Kind{model.KindSubtitle, model.KindChapter, model.KindID3, model.KindEmsg, model.KindSEI} {
		if kinds[k] == 0 {
			t.Fatalf("missing kind %v in %+v", k, kinds)
		}
	}
}

func TestChaptersAndEmsg(t *testing.T) {
	media, err := GenerateMedia(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Chapters) != 3 {
		t.Fatalf("chapters %d", len(media.Chapters))
	}
	if len(media.Emsg) != 1 || media.Emsg[0].Start != 2*clock.TickRate {
		t.Fatalf("emsg %+v", media.Emsg)
	}
}
func TestParseScenario(t *testing.T) {
	for _, s := range []string{"clean", "reorder", "duplicate", "burst", "wrap"} {
		if _, err := ParseScenario(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := ParseScenario("bogus"); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("bogus: %v", err)
	}
}
