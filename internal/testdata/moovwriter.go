package testdata

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

func u32(b *bytes.Buffer, v uint32) { binary.Write(b, binary.BigEndian, v) }
func u16(b *bytes.Buffer, v uint16) { binary.Write(b, binary.BigEndian, v) }

// writeMoov builds mvhd + two traks + udta metadata (subtitle/chapter/id3/sei
// families live in udta; emsg boxes are top-level).
func (w *boxWriter) writeMoov(video, audio TrackSpec, evs []TimedEvent, p Params) error {
	moov := w.begin("moov")
	w.writeMvhd(video)
	w.writeTrak(video, KindVideo, p)
	w.writeTrak(audio, KindAudio, p)
	w.writeUdta(evs)
	w.end(moov)
	return nil
}

func (w *boxWriter) writeMvhd(video TrackSpec) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, 0) // creation
	u32(body, 0) // modification
	u32(body, video.Timescale)
	u32(body, uint32(video.DurationTicks()))
	u32(body, 0x00010000) // rate 1.0
	body.Write([]byte{0x01, 0x00}) // volume
	body.Write(make([]byte, 10))   // reserved
	// identity matrix
	for _, m := range [9]uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		u32(body, m)
	}
	body.Write(make([]byte, 24)) // pre-defined
	u32(body, 3)                 // next track id
	f := w.begin("mvhd")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeTrak(t TrackSpec, kind Kind, p Params) {
	trak := w.begin("trak")
	w.writeTkhd(t, kind)
	mdia := w.begin("mdia")
	w.writeMdhd(t)
	w.writeHdlr(kind)
	minf := w.begin("minf")
	w.writeStbl(t, kind, p)
	w.end(minf)
	w.end(mdia)
	w.end(trak)
}

func (w *boxWriter) writeTkhd(t TrackSpec, kind Kind) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 3)) // enabled | in-movie
	u32(body, 0)
	u32(body, 0)
	u32(body, t.TrackID)
	u32(body, 0)
	u32(body, uint32(t.DurationTicks()))
	body.Write(make([]byte, 8))
	body.Write([]byte{0, 0}) // layer
	body.Write([]byte{0, 0}) // alternate
	if kind == KindAudio {
		body.Write([]byte{0x01, 0x00})
	} else {
		body.Write([]byte{0, 0})
	}
	body.Write([]byte{0, 0})
	for _, m := range [9]uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		u32(body, m)
	}
	if kind == KindVideo {
		u32(body, 1920<<16)
		u32(body, 1080<<16)
	} else {
		u32(body, 0)
		u32(body, 0)
	}
	f := w.begin("tkhd")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeMdhd(t TrackSpec) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, 0)
	u32(body, 0)
	u32(body, t.Timescale)
	u32(body, uint32(t.DurationTicks()))
	// ISO-639-2/T language packed into 16 bits: 1 pad bit + three 5-bit
	// chars. 'und' = (0x15, 0x0e, 0x04) -> 0x55c4, followed by pre_defined.
	u16(body, 0x55c4)
	u16(body, 0)
	f := w.begin("mdhd")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeHdlr(kind Kind) {
	htype := []byte("vide")
	if kind == KindAudio {
		htype = []byte("soun")
	}
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, 0)
	body.Write(htype)
	body.Write(make([]byte, 12))
	body.Write(append([]byte(string(kind)), 0))
	f := w.begin("hdlr")
	w.raw(body.Bytes())
	w.end(f)
}

// writeStbl emits stsd/stts/stsc/stsz/stco (+stss for video). The tables are
// the exact tables the ingest parser validates, so corruption is observable.
func (w *boxWriter) writeStbl(t TrackSpec, kind Kind, p Params) {
	stbl := w.begin("stbl")

	// stsd with zero entries: Probe tolerates a missing codec-specific entry
	// and our parser reads stbl tables directly.
	stsd := bytes.NewBuffer(nil)
	stsd.Write(fullBox(0, 0))
	u32(stsd, 0)
	f := w.begin("stsd")
	w.raw(stsd.Bytes())
	w.end(f)

	w.writeStts(t)
	w.writeStsc(t)
	w.writeStsz(t)
	w.writeStco(t)
	if kind == KindVideo {
		w.writeStss(t, p)
	}
	w.end(stbl)
}

func (w *boxWriter) writeStts(t TrackSpec) {
	// Collapse identical consecutive deltas into run-length entries.
	type run struct{ count, delta uint32 }
	runs := make([]run, 0)
	for _, s := range t.Samples {
		if n := len(runs); n > 0 && runs[n-1].delta == s.DurationTicks {
			runs[n-1].count++
		} else {
			runs = append(runs, run{1, s.DurationTicks})
		}
	}
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, uint32(len(runs)))
	for _, r := range runs {
		u32(body, r.count)
		u32(body, r.delta)
	}
	f := w.begin("stts")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeStsc(t TrackSpec) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, 1)
	u32(body, 1) // first chunk
	u32(body, 1) // one sample per chunk
	u32(body, 1) // description index
	f := w.begin("stsc")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeStsz(t TrackSpec) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, 0) // uniform size = 0 => table present
	u32(body, uint32(len(t.Samples)))
	for _, s := range t.Samples {
		u32(body, uint32(len(s.Payload)))
	}
	f := w.begin("stsz")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeStco(t TrackSpec) {
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, uint32(len(t.Samples)))
	base := len(w.offsets) - len(t.Samples)
	for i := range t.Samples {
		u32(body, uint32(w.offsets[base+i]))
	}
	f := w.begin("stco")
	w.raw(body.Bytes())
	w.end(f)
}

func (w *boxWriter) writeStss(t TrackSpec, p Params) {
	idx := make([]uint32, 0)
	for i, s := range t.Samples {
		if s.Keyframe {
			idx = append(idx, uint32(i+1)) // 1-based sample numbers
		}
	}
	if p.MissingKey {
		idx = idx[:0] // force "no random access point" failure mode
	}
	body := bytes.NewBuffer(nil)
	body.Write(fullBox(0, 0))
	u32(body, uint32(len(idx)))
	for _, n := range idx {
		u32(body, n)
	}
	f := w.begin("stss")
	w.raw(body.Bytes())
	w.end(f)
}

// writeUdta stores the non-emsg metadata families as custom boxes, each with
// its own timebase so ingest normalization must convert every one of them.
func (w *boxWriter) writeUdta(evs []TimedEvent) {
	udta := w.begin("udta")
	for _, e := range evs {
		if e.Kind == KindEmsg {
			continue
		}
		tag := map[Kind]string{
			KindSubtitle: "mqt t",
			KindChapter:  "mqchp",
			KindID3:      "id3 ",
			KindSEI:      "msei",
		}[e.Kind]
		body := bytes.NewBuffer(nil)
		u32(body, uint32(e.Timebase.Num))
		u32(body, uint32(e.Timebase.Den))
		u32(body, uint32(e.RawTicks))
		u32(body, e.ID)
		u32(body, uint32(e.PayloadLen))
		f := w.begin(tag)
		w.raw(body.Bytes())
		w.end(f)
	}
	w.end(udta)
}

var _ = mp4.BoxTypeMdat // keep the dependency referenced for box constants
