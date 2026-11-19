package index

import (
	"bytes"
	"testing"
)

func metas() []TrackMeta {
	return []TrackMeta{
		{TrackID: 1, SSRC: 4369, Timescale: 90000, MediaType: "video"},
		{TrackID: 2, SSRC: 8738, Timescale: 48000, MediaType: "audio"},
	}
}

func TestWriteInspectRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, metas())
	if err := w.WriteInit(); err != nil {
		t.Fatal(err)
	}
	// Two fragments, each starting with a video keyframe.
	for f := 0; f < 2; f++ {
		base := uint64(f * 3 * 3000)
		for i := 0; i < 3; i++ {
			w.AddSample(SampleIn{
				TrackID: 1, Data: []byte{0x65, byte(f), byte(i)},
				DecodeTime: base + uint64(i)*3000, Dur: 3000, Sync: i == 0,
			})
			w.AddSample(SampleIn{
				TrackID: 2, Data: []byte{0x11, byte(f), byte(i)},
				DecodeTime: base/3000*960 + uint64(i)*960, Dur: 960,
			})
		}
		if _, _, err := w.FlushFragment(); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := Inspect(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("fragments=%d", len(infos))
	}
	for i, fi := range infos {
		if fi.Seq != uint32(i+1) {
			t.Fatalf("frag seq=%d", fi.Seq)
		}
		if !fi.FirstSync[1] {
			t.Fatalf("frag %d: first video sample not sync", i)
		}
		if fi.DecodeTime[1] != uint64(i*3*3000) {
			t.Fatalf("frag %d: tfdt=%d", i, fi.DecodeTime[1])
		}
		if fi.Samples[1] != 3 || fi.Samples[2] != 3 {
			t.Fatalf("frag %d: samples=%v", i, fi.Samples)
		}
	}
}

func TestStateRestoreContinuesFragments(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, metas())
	_ = w.WriteInit()
	w.AddSample(SampleIn{TrackID: 1, Data: []byte{0x65}, DecodeTime: 0, Dur: 3000, Sync: true})
	if _, _, err := w.FlushFragment(); err != nil {
		t.Fatal(err)
	}
	w.AddSample(SampleIn{TrackID: 1, Data: []byte{0x41}, DecodeTime: 3000, Dur: 3000})
	st := w.State()
	committed := w.Offset()

	// Simulate restart: truncate to committed offset, restore state.
	truncated := buf.Bytes()[:committed]
	var out bytes.Buffer
	out.Write(truncated)
	w2 := NewWriter(&out, metas())
	w2.Restore(st, committed)
	w2.AddSample(SampleIn{TrackID: 1, Data: []byte{0x65}, DecodeTime: 6000, Dur: 3000, Sync: true})
	if _, _, err := w2.FlushFragment(); err != nil {
		t.Fatal(err)
	}
	infos, err := Inspect(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[1].Seq != 2 {
		t.Fatalf("infos=%+v", infos)
	}
	if infos[1].Samples[1] != 2 {
		t.Fatalf("restored fragment samples=%d", infos[1].Samples[1])
	}
}
