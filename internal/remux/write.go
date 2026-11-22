package remux

import (
	"bytes"
	"encoding/binary"

	"archive/internal/clock"
	"archive/internal/demux"
	"archive/internal/fault"
)

// Input pairs a demuxed track with absolute decode times, one per
// sample, expressed in the track timescale. The caller owns the
// timeline: write never re-derives or rounds timestamps.
type Input struct {
	Track demux.Track
	DTS   []int64
}

// TrackInfo is the per-track table entry describing one output track.
type TrackInfo struct {
	ID        uint32
	Role      string
	Lang      string
	Codec     string
	Timescale uint32
	Samples   int
	Start     clock.Rational // first presentation timestamp
	End       clock.Rational // end of last sample
}

// Info summarises one remuxed segment.
type Info struct {
	Tracks []TrackInfo
	Bytes  int64
	// SyncMs is the end-to-end audio/video start offset in milliseconds;
	// zero when the segment is single-track.
	SyncMs float64
}

// Write builds a self-contained fMP4 for inputs, which must already be
// in output order. Output track IDs are 1..len(inputs).
func Write(inputs []Input) ([]byte, Info, error) {
	if len(inputs) == 0 {
		return nil, Info{}, fault.Newf(fault.Fatal, "remux", "", "no tracks to write")
	}
	info := Info{Tracks: make([]TrackInfo, len(inputs))}
	tracks := make([]demux.Track, len(inputs))
	for i, in := range inputs {
		tr := in.Track
		if tr.Timescale == 0 {
			return nil, Info{}, fault.Newf(fault.Fatal, "remux", "", "track %d has zero timescale", tr.ID)
		}
		if len(tr.Entry) == 0 {
			return nil, Info{}, fault.Newf(fault.Fatal, "remux", "", "track %d has no sample entry", tr.ID)
		}
		if len(in.DTS) != len(tr.Samples) {
			return nil, Info{}, fault.Newf(fault.Fatal, "remux", "", "track %d: %d timestamps for %d samples",
				tr.ID, len(in.DTS), len(tr.Samples))
		}
		for j := 1; j < len(in.DTS); j++ {
			if in.DTS[j] < in.DTS[j-1] {
				return nil, Info{}, fault.Newf(fault.Fatal, "remux", "",
					"track %d: decode time moves backwards at sample %d", tr.ID, j)
			}
		}
		tracks[i] = tr
		ti, err := describe(uint32(i+1), tr, in.DTS)
		if err != nil {
			return nil, Info{}, err
		}
		info.Tracks[i] = ti
	}
	info.SyncMs = syncDelta(info.Tracks)

	var b bytes.Buffer
	b.Write(ftyp())
	b.Write(moov(tracks))

	// Build one moof + mdat containing every track's samples.
	moof, mdat, err := fragment(inputs)
	if err != nil {
		return nil, Info{}, err
	}
	b.Write(moof)
	b.Write(mdat)
	info.Bytes = int64(b.Len())
	return b.Bytes(), info, nil
}

func describe(id uint32, tr demux.Track, ds []int64) (TrackInfo, error) {
	ti := TrackInfo{
		ID: id, Role: roleOf(tr.Handler), Lang: langOf(tr.Lang),
		Codec: tr.Codec, Timescale: tr.Timescale, Samples: len(tr.Samples),
	}
	if len(tr.Samples) == 0 {
		return ti, nil
	}
	// Presentation start = first DTS + its CTS offset.
	first := ds[0] + tr.Samples[0].CTSOff
	start, err := clock.New(first, int64(tr.Timescale))
	if err != nil {
		return ti, fault.Wrap(fault.Fatal, "remux", tr.Codec, err)
	}
	ti.Start = start
	last := ds[len(ds)-1]
	end, err := clock.New(last+int64(tr.Samples[len(ds)-1].Dur), int64(tr.Timescale))
	if err != nil {
		return ti, fault.Wrap(fault.Fatal, "remux", tr.Codec, err)
	}
	ti.End = end
	return ti, nil
}

// syncDelta returns the absolute audio/video presentation-start gap in
// milliseconds, or 0 for single-track segments.
func syncDelta(tis []TrackInfo) float64 {
	var v, a *TrackInfo
	for i := range tis {
		if tis[i].Samples == 0 {
			continue
		}
		switch tis[i].Role {
		case "video":
			if v == nil {
				v = &tis[i]
			}
		case "audio":
			if a == nil {
				a = &tis[i]
			}
		}
	}
	if v == nil || a == nil {
		return 0
	}
	d, err := v.Start.Sub(a.Start)
	if err != nil {
		return 0
	}
	f := d.Float64() * 1000
	if f < 0 {
		f = -f
	}
	return f
}

func roleOf(handler string) string {
	switch handler {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	default:
		return "aux"
	}
}

func langOf(l string) string {
	if l == "" {
		return "und"
	}
	return l
}

// --- box construction -------------------------------------------------

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func container(typ string, children ...[]byte) []byte {
	var body bytes.Buffer
	for _, c := range children {
		body.Write(c)
	}
	return box(typ, body.Bytes())
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	body := make([]byte, 4, 4+len(payload))
	body[0] = version
	body[1] = byte(flags >> 16)
	body[2] = byte(flags >> 8)
	body[3] = byte(flags)
	return box(typ, append(body, payload...))
}

func ftyp() []byte {
	var p bytes.Buffer
	p.WriteString("iso6")
	var minor [4]byte
	p.Write(minor[:])
	for _, brand := range []string{"iso6", "mp41", "dash"} {
		p.WriteString(brand)
	}
	return box("ftyp", p.Bytes())
}

func moov(tracks []demux.Track) []byte {
	var mvhdBody bytes.Buffer
	mvhdBody.Write([]byte{0, 0, 0, 0}) // creation
	mvhdBody.Write([]byte{0, 0, 0, 0}) // modification
	u32(&mvhdBody, 1000)               // timescale
	u32(&mvhdBody, 0)                  // duration (fragmented)
	u32(&mvhdBody, 0x00010000)         // rate
	u16(&mvhdBody, 0x0100)             // volume
	u16(&mvhdBody, 0)
	u32(&mvhdBody, 0)
	u32(&mvhdBody, 0)
	writeMatrix(&mvhdBody)
	for i := 0; i < 6; i++ {
		u32(&mvhdBody, 0)
	}
	u32(&mvhdBody, uint32(len(tracks)+1)) // next track ID
	mvhd := fullBox("mvhd", 0, 0, mvhdBody.Bytes())

	children := [][]byte{mvhd}
	var trexes [][]byte
	for i := range tracks {
		id := uint32(i + 1)
		children = append(children, trak(id, tracks[i]))
		var trexBody bytes.Buffer
		u32(&trexBody, id)
		u32(&trexBody, 1)
		u32(&trexBody, 0)
		u32(&trexBody, 0)
		u32(&trexBody, 0)
		trexes = append(trexes, fullBox("trex", 0, 0, trexBody.Bytes()))
	}
	children = append(children, container("mvex", trexes...))
	return container("moov", children...)
}

func trak(id uint32, tr demux.Track) []byte {
	var tkhd bytes.Buffer
	tkhd.Write([]byte{0, 0, 0, 0}) // creation
	tkhd.Write([]byte{0, 0, 0, 0}) // modification
	u32(&tkhd, id)
	u32(&tkhd, 0) // reserved
	u32(&tkhd, 0) // duration
	u32(&tkhd, 0)
	u32(&tkhd, 0)
	u16(&tkhd, 0) // layer
	u16(&tkhd, 0) // alternate group
	if tr.Handler == "soun" {
		u16(&tkhd, 0x0100) // volume
	} else {
		u16(&tkhd, 0)
	}
	u16(&tkhd, 0) // reserved
	writeMatrix(&tkhd)
	u32(&tkhd, uint32(tr.Width)<<16)
	u32(&tkhd, uint32(tr.Height)<<16)

	var mdhd bytes.Buffer
	mdhd.Write([]byte{0, 0, 0, 0})
	mdhd.Write([]byte{0, 0, 0, 0})
	u32(&mdhd, tr.Timescale)
	u32(&mdhd, 0) // duration (fragmented)
	u16(&mdhd, packLang(langOf(tr.Lang)))
	u16(&mdhd, 0) // pre-defined

	var hdlrBody bytes.Buffer
	u32(&hdlrBody, 0) // pre-defined
	handler := tr.Handler
	if len(handler) != 4 {
		handler = "auxv"
	}
	hdlrBody.WriteString(handler)
	u32(&hdlrBody, 0)
	u32(&hdlrBody, 0)
	u32(&hdlrBody, 0)
	hdlrBody.WriteString("archive")
	hdlrBody.WriteByte(0)
	hdlrBox := fullBox("hdlr", 0, 0, hdlrBody.Bytes())

	var stsdBody bytes.Buffer
	u32(&stsdBody, 1) // entry count
	stsdBody.Write(tr.Entry)
	stsd := fullBox("stsd", 0, 0, stsdBody.Bytes())

	var mediaHeader []byte
	if tr.Handler == "soun" {
		var smhd bytes.Buffer
		u16(&smhd, 0) // balance
		u16(&smhd, 0) // reserved
		mediaHeader = fullBox("smhd", 0, 0, smhd.Bytes())
	} else {
		var vmhd bytes.Buffer
		u16(&vmhd, 0) // graphics mode
		u16(&vmhd, 0)
		u16(&vmhd, 0)
		u16(&vmhd, 0)
		mediaHeader = fullBox("vmhd", 0, 1, vmhd.Bytes())
	}

	var drefBody bytes.Buffer
	u32(&drefBody, 1)
	drefBody.Write(fullBox("url ", 0, 1, nil)) // self-contained
	dinf := container("dinf", fullBox("dref", 0, 0, drefBody.Bytes()))

	minf := container("minf", mediaHeader, dinf, container("stbl", stsd))
	mdia := container("mdia", fullBox("mdhd", 0, 0, mdhd.Bytes()), hdlrBox, minf)
	return container("trak", fullBox("tkhd", 0, 7, tkhd.Bytes()), mdia)
}
