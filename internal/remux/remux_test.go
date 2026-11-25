package remux_test

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"mediqc/internal/audit"
	"mediqc/internal/media"
	"mediqc/internal/remux"
	"mediqc/internal/ts"
)

func tracks() []media.Track {
	return []media.Track{
		{ID: 1, PID: 0x100, Kind: media.TrackVideo, Codec: media.CodecH264, Timescale: 90000},
		{ID: 2, PID: 0x101, Kind: media.TrackAudio, Codec: media.CodecAAC, Timescale: 90000},
	}
}

func samples() []media.Sample {
	return []media.Sample{
		{TrackID: 1, PID: 0x100, Index: 0, PTS: 0, DTS: 0, Duration: 3000, Data: []byte{0, 0, 1, 0x65, 'v', 1}},
		{TrackID: 2, PID: 0x101, Index: 1, PTS: 0, DTS: 0, Duration: 2160, Data: []byte{0xff, 0xf1, 'a', 1}},
		{TrackID: 1, PID: 0x100, Index: 2, PTS: 3000, DTS: 3000, Duration: 3000, Data: []byte{0, 0, 1, 0x61, 'v', 2}},
		{TrackID: 2, PID: 0x101, Index: 3, PTS: 2160, DTS: 2160, Duration: 2160, Data: []byte{0xff, 0xf1, 'a', 2}},
	}
}

func TestTSOutputPreservesSampleHashes(t *testing.T) {
	in := samples()
	b, err := remux.BuildTS(tracks(), in, remux.TSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, merr := ts.Demux(b, media.DefaultLimits(), media.NewCollector(64))
	if merr != nil {
		t.Fatal(merr)
	}
	if len(res.Samples) != len(in) {
		t.Fatalf("samples=%d want=%d", len(res.Samples), len(in))
	}
	for i := range in {
		if res.Samples[i].Hash() != in[i].Hash() {
			t.Fatalf("sample %d hash changed", i)
		}
	}
}

func TestFMP4OutputCanBeAuditedSampleBySample(t *testing.T) {
	in := samples()
	frags := []remux.Fragment{
		{Sequence: 1, TrackID: 1, BaseDecodeTime: 0, Samples: []media.Sample{in[0], in[2]}},
		{Sequence: 2, TrackID: 2, BaseDecodeTime: 0, Samples: []media.Sample{in[1], in[3]}},
	}
	b, err := remux.BuildFMP4(tracks(), frags)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.Probe(bytes.NewReader(b)); err != nil {
		t.Fatalf("go-mp4 probe rejected output: %v", err)
	}
	got, merr := audit.ExtractFMP4Samples(b)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(got) != len(in) {
		t.Fatalf("fMP4 samples=%d want=%d", len(got), len(in))
	}
	want := map[string]int{}
	for _, s := range in {
		want[s.Hash()]++
	}
	for _, s := range got {
		h := (media.Sample{Data: s.Data}).Hash()
		if want[h] == 0 {
			t.Fatalf("unexpected fMP4 sample hash %s", h)
		}
		want[h]--
	}
}
