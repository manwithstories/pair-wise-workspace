package mp4io

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"example.com/medialab/archive/internal/model"
)

func TestInitSegmentStructure(t *testing.T) {
	for _, codec := range []model.Codec{model.CodecOpus, model.CodecAAC} {
		cfg := Config{Rep: "64k", Codec: codec, SampleRate: 48000, Channels: 2, Bitrate: 64000}
		data, err := cfg.InitSegment()
		if err != nil {
			t.Fatalf("%s init: %v", codec, err)
		}
		rd := &memReader{data: data}
		var types []string
		var mdhdTimeScale uint32
		_, err = mp4.ReadBoxStructure(rd, func(h *mp4.ReadHandle) (interface{}, error) {
			types = append(types, h.BoxInfo.Type.String())
			switch h.BoxInfo.Type {
			case mp4.BoxTypeMdhd():
				box, _, err := h.ReadPayload()
				if err != nil {
					return nil, err
				}
				mdhdTimeScale = box.(*mp4.Mdhd).Timescale
			}
			if h.BoxInfo.Type == mp4.BoxTypeMoov() || h.BoxInfo.Type == mp4.BoxTypeTrak() ||
				h.BoxInfo.Type == mp4.BoxTypeMdia() || h.BoxInfo.Type == mp4.BoxTypeMinf() ||
				h.BoxInfo.Type == mp4.BoxTypeStbl() || h.BoxInfo.Type == mp4.BoxTypeMvex() ||
				h.BoxInfo.Type == mp4.BoxTypeDinf() || h.BoxInfo.Type == mp4.BoxTypeDref() ||
				h.BoxInfo.Type == mp4.BoxTypeStsd() {
				_, err := h.Expand()
				return nil, err
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("%s parse: %v", codec, err)
		}
		if mdhdTimeScale != Timescale {
			t.Fatalf("%s timescale = %d", codec, mdhdTimeScale)
		}
		want := []string{"ftyp", "moov", "mvhd", "trak", "tkhd", "mdia", "mdhd", "hdlr", "minf", "smhd", "dinf", "dref", "url ", "stbl", "stsd"}
		for _, w := range want {
			found := false
			for _, got := range types {
				if got == w {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s missing box %s in %v", codec, w, types)
			}
		}
	}
}

func TestMediaSegmentRoundTrip(t *testing.T) {
	samples := []Sample{
		{DurationUS: 20000, Payload: bytes.Repeat([]byte{0xAB}, 40)},
		{DurationUS: 20000, Payload: bytes.Repeat([]byte{0xCD}, 60)},
		{DurationUS: 10000, Payload: bytes.Repeat([]byte{0xEF}, 10)},
	}
	seg, err := MediaSegment(samples, 5_000_000, 7)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Verify(seg)
	if err != nil {
		t.Fatal(err)
	}
	if v.TrackID != 1 || v.Sequence != 7 || v.BaseDecodeUS != 5_000_000 {
		t.Fatalf("verified = %+v", v)
	}
	if len(v.Samples) != 3 || v.TotalDuration != 50000 {
		t.Fatalf("samples = %+v total %d", v.Samples, v.TotalDuration)
	}
	if v.DataBytes != 110 {
		t.Fatalf("data bytes = %d", v.DataBytes)
	}
	if v.Samples[2].DecodeTimeUS != 5_040_000 {
		t.Fatalf("sample 2 decode = %d", v.Samples[2].DecodeTimeUS)
	}
}

func TestMediaSegmentRejects(t *testing.T) {
	if _, err := MediaSegment(nil, 0, 0); err == nil {
		t.Fatal("empty accepted")
	}
	if _, err := MediaSegment([]Sample{{DurationUS: 0, Payload: []byte{1}}}, 0, 0); err == nil {
		t.Fatal("zero duration accepted")
	}
	if _, err := MediaSegment([]Sample{{DurationUS: 1, Payload: nil}}, 0, 0); err == nil {
		t.Fatal("empty payload accepted")
	}
	if _, err := MediaSegment([]Sample{{DurationUS: 1, Payload: []byte{1}}}, -1, 0); err == nil {
		t.Fatal("negative decode time accepted")
	}
}

func TestVerifyRejectsTruncated(t *testing.T) {
	seg, err := MediaSegment([]Sample{{DurationUS: 20000, Payload: []byte{1, 2, 3}}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(seg[:len(seg)-2]); err == nil {
		t.Fatal("truncated segment verified")
	}
	if _, err := Verify(nil); err == nil {
		t.Fatal("empty segment verified")
	}
}

func TestInitSegmentValidates(t *testing.T) {
	bad := []Config{
		{},
		{Rep: "x", Codec: "mp3", SampleRate: 48000, Channels: 2, Bitrate: 64000},
		{Rep: "x", Codec: model.CodecOpus, SampleRate: 0, Channels: 2, Bitrate: 64000},
		{Rep: "x", Codec: model.CodecOpus, SampleRate: 48000, Channels: 0, Bitrate: 64000},
		{Rep: "x", Codec: model.CodecOpus, SampleRate: 48000, Channels: 2, Bitrate: 0},
	}
	for i, c := range bad {
		if _, err := c.InitSegment(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
