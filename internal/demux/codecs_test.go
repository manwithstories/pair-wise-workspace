package demux

import (
	"bytes"
	"io"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

type stubWS struct {
	b   []byte
	pos int64
}

func (w *stubWS) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if int(end) > len(w.b) {
		w.b = append(w.b, make([]byte, int(end)-len(w.b))...)
	}
	copy(w.b[w.pos:end], p)
	w.pos = end
	return len(p), nil
}
func (w *stubWS) Seek(o int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = o
	case io.SeekCurrent:
		w.pos += o
	case io.SeekEnd:
		w.pos = int64(len(w.b)) + o
	}
	return w.pos, nil
}

func stubLeaf(w *mp4.Writer, b mp4.IImmutableBox) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: b.GetType()}); err != nil {
		return err
	}
	if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}

func stubInit(t *testing.T, entry mp4.IImmutableBox, handler string, ts uint32) []byte {
	t.Helper()
	ws := &stubWS{}
	mw := mp4.NewWriter(ws)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		t.Fatal(err)
	}
	if err := stubLeaf(mw, &mp4.Mvhd{Timescale: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTrak()}); err != nil {
		t.Fatal(err)
	}
	tkhd := &mp4.Tkhd{TrackID: 7}
	tkhd.SetFlags(0x3)
	if err := stubLeaf(mw, tkhd); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdia()}); err != nil {
		t.Fatal(err)
	}
	if err := stubLeaf(mw, &mp4.Mdhd{Timescale: ts, Language: EncodeLanguage("eng")}); err != nil {
		t.Fatal(err)
	}
	h := &mp4.Hdlr{Name: "p"}
	copy(h.HandlerType[:], handler)
	if err := stubLeaf(mw, h); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMinf()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStbl()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeStsd()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.Marshal(mw, &mp4.Stsd{EntryCount: 1}, mp4.Context{}); err != nil {
		t.Fatal(err)
	}
	if err := stubLeaf(mw, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // stsd
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // stbl
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // minf
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // mdia
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // trak
	if _, err := mw.EndBox(); err != nil {
		t.Fatal(err)
	} // moov
	if !bytes.Contains(ws.b, []byte("stsd")) {
		t.Fatal("builder produced no stsd")
	}
	return ws.b
}

func TestParseInitClassifiesAllCodecFamilies(t *testing.T) {
	vc := func(fourcc string) *mp4.VisualSampleEntry {
		e := &mp4.VisualSampleEntry{Width: 320, Height: 180}
		e.SetType(mp4.StrToBoxType(fourcc))
		e.DataReferenceIndex = 1
		return e
	}
	ac := func(fourcc string) *mp4.AudioSampleEntry {
		e := &mp4.AudioSampleEntry{ChannelCount: 2, SampleSize: 16}
		e.SetType(mp4.StrToBoxType(fourcc))
		e.DataReferenceIndex = 1
		return e
	}
	wvtt := &mp4.WVTTSampleEntry{}
	wvtt.SetType(mp4.BoxTypeWvtt())
	wvtt.DataReferenceIndex = 1
	cases := []struct {
		name    string
		entry   mp4.IImmutableBox
		handler string
		want    string
		chans   uint16
	}{
		{"avc", vc("avc1"), "vide", "avc1", 0},
		{"hevc-hvc1", vc("hvc1"), "vide", "hvc1", 0},
		{"hevc-hev1", vc("hev1"), "vide", "hev1", 0},
		{"av1", vc("av01"), "vide", "av01", 0},
		{"aac", ac("mp4a"), "soun", "mp4a", 2},
		{"opus", ac("Opus"), "soun", "Opus", 2},
		{"wvtt", wvtt, "text", "wvtt", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := stubInit(t, c.entry, c.handler, 48000)
			info, fault := ParseInit(data)
			if fault != nil {
				t.Fatalf("ParseInit fault: %v", fault)
			}
			if len(info.Tracks) != 1 {
				t.Fatalf("tracks=%d", len(info.Tracks))
			}
			ti := info.Tracks[0]
			if ti.Codec != c.want {
				t.Fatalf("codec=%q want %q (handler=%q)", ti.Codec, c.want, ti.Handler)
			}
			if c.chans != 0 && ti.Channels != c.chans {
				t.Fatalf("channels=%d want %d", ti.Channels, c.chans)
			}
			if ti.Language != "eng" || ti.Timescale != 48000 {
				t.Fatalf("lang=%q ts=%d", ti.Language, ti.Timescale)
			}
		})
	}
}
