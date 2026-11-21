package remux

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

func TestTreeParsesToSampleTables(t *testing.T) {
	plan := testPlan(t)
	out := &memWriteSeeker{}
	if _, err := Write(out, plan); err != nil {
		t.Fatal(err)
	}
	type found struct {
		stts, stsc, stsz, stco, stsd, stss, ctts bool
		entry                                    string
	}
	got := map[string]*found{}
	r := bytes.NewReader(out.buf)
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeTrak() {
			got["trak"+string(rune('0'+len(got)))] = &found{}
		}
		if h.BoxInfo.IsSupportedType() {
			if _, _, err := h.ReadPayload(); err != nil {
				return nil, err
			}
		}
		return h.Expand()
	})
	if err != nil {
		t.Fatalf("structure: %v", err)
	}
	probe, err := mp4.Probe(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(probe.Tracks))
	}
	for i, tr := range probe.Tracks {
		if len(tr.Samples) != 25 {
			t.Fatalf("track %d samples=%d", i, len(tr.Samples))
		}
		if len(tr.Chunks) == 0 {
			t.Fatalf("track %d has no chunks", i)
		}
	}
	if probe.Tracks[0].Codec != mp4.CodecUnknown && probe.Tracks[1].Codec != mp4.CodecUnknown {
		t.Logf("codecs: %v %v", probe.Tracks[0].Codec, probe.Tracks[1].Codec)
	}
}
