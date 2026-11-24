package mp4_test

import (
	"bytes"
	"errors"
	"testing"

	gomp4 "github.com/abema/go-mp4"

	mp4writer "mediad/internal/mp4"
	"mediad/internal/pcm"
)

func TestRenderDeterministicAndBoxStructureReadable(t *testing.T) {
	a, err := pcm.NewAudio(48000, 2, 9600)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.Samples {
		a.Samples[i] = 0.125
	}
	b1, sha1, err := mp4writer.Render(a, 4800)
	if err != nil {
		t.Fatal(err)
	}
	b2, sha2, err := mp4writer.Render(a, 4800)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) || sha1 != sha2 || len(sha1) != 64 {
		t.Fatal("render is not deterministic")
	}

	kinds := map[string]int{}
	_, err = gomp4.ReadBoxStructure(bytes.NewReader(b1), func(h *gomp4.ReadHandle) (any, error) {
		kinds[h.BoxInfo.Type.String()]++
		if h.BoxInfo.Type == gomp4.BoxTypeTrun() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*gomp4.Trun)
			if trun.DataOffset != 100 {
				t.Fatalf("trun data offset=%d want 100", trun.DataOffset)
			}
			if trun.SampleCount != 1 || trun.Entries[0].SampleSize != 19200 {
				t.Fatalf("unexpected trun entry: %+v", trun)
			}
		}
		if _, err := h.Expand(); err != nil && !errors.Is(err, gomp4.ErrBoxInfoNotFound) {
			return nil, err
		}
		return h.BoxInfo.Type.String(), nil
	})
	if err != nil {
		t.Fatalf("go-mp4 cannot read rendered structure: %v", err)
	}
	for _, typ := range []string{"ftyp", "moov", "trak", "mdia", "stbl", "moof", "traf", "trun", "mdat"} {
		if kinds[typ] == 0 {
			t.Fatalf("missing box %s in %v", typ, kinds)
		}
	}
	if kinds["moof"] != 2 || kinds["mdat"] != 2 {
		t.Fatalf("fragment counts wrong: %v", kinds)
	}
}

func TestRenderRejectsWrongFormat(t *testing.T) {
	a, _ := pcm.NewAudio(44100, 6, 4410)
	if _, _, err := mp4writer.Render(a, 441); err == nil {
		t.Fatal("expected format rejection")
	}
}
