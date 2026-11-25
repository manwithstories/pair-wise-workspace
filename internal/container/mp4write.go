package container

import (
	"strconv"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// WriteMP4 re-muxes a parsed File with the resolved description written back
// into both description layers. Sample payloads are copied unchanged and their
// relative layout is preserved, so re-parsing the output reproduces every
// field the resolver understood.
//
// The rewrite is structural, not textual: timing tables, sizes and chunk
// layout are recomputed from the track model rather than copied, which is what
// makes round-trip comparison meaningful.
func WriteMP4(f *File) ([]byte, error) {
	if f == nil || f.Kind != model.MP4 {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false, "WriteMP4: not an MP4 file")
	}
	if len(f.Tracks) == 0 {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false, "WriteMP4: no tracks")
	}
	w := &boxWriter{}
	brand := f.Brand
	if brand == "" {
		brand = "isom"
	}
	// ftyp
	b := w.box("ftyp")
	w.bytes([]byte(padBrand(brand)))
	w.u32(0x200)
	w.bytes([]byte("isom"))
	w.bytes([]byte("iso2"))
	w.bytes([]byte("mjpg"))
	w.endBox(b)

	// moov is written with placeholder chunk offsets first; because its size
	// does not depend on the offset values, a second pass can fill them in.
	mf := buildMoovWriter(f)
	moov := mf.bytes
	mdatOffset := int64(len(w.buf) + len(moov) + 8)

	// Essence is concatenated in track order, sample order.
	var mdat []byte
	offsets := make([][]int64, len(f.Tracks))
	pos := mdatOffset
	for ti, t := range f.Tracks {
		offsets[ti] = make([]int64, len(t.Samples))
		for si, s := range t.Samples {
			offsets[ti][si] = pos
			payload, err := t.Payload(f.Data, s)
			if err != nil {
				// A sample with no essence data is written as an empty range;
				// the gap is already recorded as a fault on the parse side.
				payload = nil
			}
			mdat = append(mdat, payload...)
			pos += int64(len(payload))
		}
	}
	mf.applyOffsets(offsets)
	moov = mf.bytes
	w.bytes(moov)
	md := w.box("mdat")
	w.bytes(mdat)
	w.endBox(md)
	return w.buf, nil
}

func padBrand(s string) string {
	if len(s) >= 4 {
		return s[:4]
	}
	return s + "    "[len(s):]
}

// moovWriter builds a moov with chunk offset placeholders.
type moovWriter struct {
	bytes []byte
	// offsetPatches records where each chunk offset value lives.
	offsetPatches [][]int
	// trackOrder maps track index to the location of its stco box.
	trackOrder int
}

// buildMoovWriter serialises the movie header and every track. stco entries
// are written as zero and patched afterwards.
func buildMoovWriter(f *File) *moovWriter {
	mw := &moovWriter{}
	w := &boxWriter{}
	moovStart := w.box("moov")

	// Compute movie duration in the 1000-tick movie timescale.
	var maxDur int64
	for _, t := range f.Tracks {
		d := t.Duration
		if t.Timebase != (model.Timebase{}) {
			if ms, err := t.Timebase.ConvertTo(t.Duration, model.Millis); err == nil {
				d = ms
			}
		}
		if d > maxDur {
			maxDur = d
		}
	}
	mvhd := w.fullBox("mvhd", 0, 0)
	w.u32(0)
	w.u32(0)
	w.u32(1000)
	w.u32(uint32(clampU32(maxDur)))
	w.u32(0x00010000)
	w.u16(0x0100)
	w.u16(0)
	w.u32(0)
	w.u32(0)
	w.matrix(0)
	w.pad(24)
	w.u32(uint32(len(f.Tracks) + 1))
	w.endBox(mvhd)

	for _, t := range f.Tracks {
		writeTrak(w, f, t, mw)
	}
	w.endBox(moovStart)
	mw.bytes = w.buf
	return mw
}

// applyOffsets rewrites the zero chunk offsets in place. Because there is one
// chunk per track holding every sample, each stco has exactly one entry.
func (mw *moovWriter) applyOffsets(offsets [][]int64) {
	for ti, locs := range mw.offsetPatches {
		for _, at := range locs {
			var v int64
			if ti < len(offsets) && len(offsets[ti]) > 0 {
				v = offsets[ti][0]
			}
			binaryPutU32(mw.bytes, at, uint32(clampU32(v)))
		}
	}
}

func writeTrak(w *boxWriter, f *File, t *model.Track, mw *moovWriter) {
	trak := w.box("trak")
	// tkhd
	tkhd := w.fullBox("tkhd", 0, 3)
	w.u32(0)
	w.u32(0)
	w.u32(t.ID)
	w.u32(0)
	var durMs int64
	if t.Timebase != (model.Timebase{}) {
		if ms, err := t.Timebase.ConvertTo(t.Duration, model.Millis); err == nil {
			durMs = ms
		}
	}
	w.u32(uint32(clampU32(durMs)))
	w.u32(0)
	w.u32(0)
	w.u16(0)
	w.u16(0)
	if t.Kind == model.Audio {
		w.u16(0x0100)
	} else {
		w.u16(0)
	}
	w.u16(0)
	w.matrix(t.Resolved.Rotation)
	w.u32(uint32(t.Resolved.Width) << 16)
	w.u32(uint32(t.Resolved.Height) << 16)
	w.endBox(tkhd)

	// The track-level colr box is the container description layer and is
	// written separately from the sample-entry colr.
	if t.Claims.Container["primaries"] != "" {
		c := containerColr(t)
		w.bytes(c)
	}

	mdia := w.box("mdia")
	mdhd := w.fullBox("mdhd", 0, 0)
	w.u32(0)
	w.u32(0)
	w.u32(uint32(t.Timebase.Num))
	w.u32(uint32(clampU32(t.Duration)))
	w.u16(0x55c4)
	w.u16(0)
	w.endBox(mdhd)

	hdlr := w.fullBox("hdlr", 0, 0)
	w.u32(0)
	switch t.Kind {
	case model.Audio:
		w.bytes([]byte("soun"))
	case model.Video:
		w.bytes([]byte("vide"))
	default:
		w.bytes([]byte("meta"))
	}
	w.pad(12)
	w.bytes([]byte("RelayHandler\x00"))
	w.endBox(hdlr)

	minf := w.box("minf")
	if t.Kind == model.Audio {
		smhd := w.fullBox("smhd", 0, 0)
		w.u16(0)
		w.u16(0)
		w.endBox(smhd)
	} else {
		vmhd := w.fullBox("vmhd", 0, 1)
		w.u16(0)
		w.u16(0)
		w.u16(0)
		w.u16(0)
		w.endBox(vmhd)
	}
	dinf := w.box("dinf")
	dref := w.fullBox("dref", 0, 0)
	w.u32(1)
	url := w.fullBox("url ", 0, 1)
	w.endBox(url)
	w.endBox(dref)
	w.endBox(dinf)

	stbl := w.box("stbl")
	writeStsd(w, t)
	writeStts(w, t)
	writeStss(w, t)
	writeStsz(w, t)
	writeStsc(w, t)
	// stco: record the absolute position of the offset value for patching.
	stco := w.fullBox("stco", 0, 0)
	w.u32(1)
	at := len(w.buf)
	w.u32(0)
	w.endBox(stco)
	mw.offsetPatches = append(mw.offsetPatches, []int{at})
	w.endBox(stbl)
	w.endBox(minf)
	w.endBox(mdia)
	w.endBox(trak)
}

func writeStsd(w *boxWriter, t *model.Track) {
	stsd := w.fullBox("stsd", 0, 0)
	w.u32(1)
	fourCC := codecFourCC(t.Codec, t.Kind)
	if t.Kind == model.Audio {
		entry := w.box(fourCC)
		w.pad(6)
		w.u16(1)
		w.pad(8)
		w.u16(2)
		w.u16(16)
		w.u16(0)
		w.u16(0)
		w.u32(48000 << 16)
		w.endBox(entry)
	} else {
		entry := w.box(fourCC)
		w.pad(6)
		w.u16(1)
		w.u16(0)
		w.u16(0)
		w.u32(0)
		w.u32(0)
		w.u32(0)
		w.u16(uint16(t.Resolved.Width))
		w.u16(uint16(t.Resolved.Height))
		w.u32(0x00480000)
		w.u32(0x00480000)
		w.u32(0)
		w.u16(1)
		w.pad(32) // compressor name
		w.u16(0x0018)
		w.u16(0xffff)
		// Bitstream layer: colour and aperture live in the sample entry.
		if t.Resolved.ColorKnown {
			w.colr(t.Resolved.Primaries, t.Resolved.Transfer, t.Resolved.Matrix, t.Resolved.FullRange)
		}
		r := t.Resolved.Crop
		if !r.Empty() {
			cw := t.Resolved.Width - r.Left - r.Right
			ch := t.Resolved.Height - r.Top - r.Bottom
			if cw > 0 && ch > 0 {
				w.clap(cw, ch)
			}
		}
		w.endBox(entry)
	}
	w.endBox(stsd)
}

func writeStts(w *boxWriter, t *model.Track) {
	stts := w.fullBox("stts", 0, 0)
	// Run-length encode the preserved sample durations.
	type run struct {
		count uint32
		delta int64
	}
	var runs []run
	for _, s := range t.Samples {
		if n := len(runs); n > 0 && runs[n-1].delta == s.Duration {
			runs[n-1].count++
			continue
		}
		runs = append(runs, run{count: 1, delta: s.Duration})
	}
	w.u32(uint32(len(runs)))
	for _, r := range runs {
		w.u32(r.count)
		w.u32(uint32(clampU32(r.delta)))
	}
	w.endBox(stts)
}

func writeStss(w *boxWriter, t *model.Track) {
	allSync := true
	n := 0
	for _, s := range t.Samples {
		if !s.Sync {
			allSync = false
		} else {
			n++
		}
	}
	if allSync {
		return
	}
	stss := w.fullBox("stss", 0, 0)
	w.u32(uint32(n))
	for _, s := range t.Samples {
		if s.Sync {
			w.u32(uint32(s.Index + 1))
		}
	}
	w.endBox(stss)
}

func writeStsz(w *boxWriter, t *model.Track) {
	stsz := w.fullBox("stsz", 0, 0)
	uniform := int64(-1)
	for _, s := range t.Samples {
		if uniform == -1 {
			uniform = s.Size
			continue
		}
		if s.Size != uniform {
			uniform = 0
			break
		}
	}
	if uniform > 0 {
		w.u32(uint32(uniform))
	} else {
		w.u32(0)
	}
	w.u32(uint32(len(t.Samples)))
	if uniform <= 0 {
		for _, s := range t.Samples {
			w.u32(uint32(clampU32(s.Size)))
		}
	}
	w.endBox(stsz)
}

func writeStsc(w *boxWriter, t *model.Track) {
	stsc := w.fullBox("stsc", 0, 0)
	w.u32(1)
	w.u32(1)
	w.u32(uint32(len(t.Samples)))
	w.u32(1)
	w.endBox(stsc)
}

func binaryPutU32(buf []byte, at int, v uint32) {
	if at < 0 || at+4 > len(buf) {
		return
	}
	buf[at] = byte(v >> 24)
	buf[at+1] = byte(v >> 16)
	buf[at+2] = byte(v >> 8)
	buf[at+3] = byte(v)
}

func clampU32(v int64) int64 {
	const max = int64(^uint32(0))
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// codecFourCC maps a model codec back to its four-character sample entry code.
func codecFourCC(c model.Codec, kind model.TrackKind) string {
	switch c {
	case model.CodecMJPEG:
		return "mjpg"
	case model.CodecH264:
		return "avc1"
	case model.CodecAAC:
		return "mp4a"
	case model.CodecPCM:
		return "lpcm"
	}
	if kind == model.Audio {
		return "mp4a"
	}
	return "mjpg"
}

// containerColr rebuilds the track-level colr box from the container-layer
// claims recorded during parsing.
func containerColr(t *model.Track) []byte {
	parse := func(key string) uint16 {
		n, err := strconv.Atoi(t.Claims.Container[key])
		if err != nil || n < 0 || n > 0xffff {
			return 0
		}
		return uint16(n)
	}
	full := t.Claims.Container["range"] == "full"
	w := &boxWriter{}
	w.colr(parse("primaries"), parse("transfer"), parse("matrix"), full)
	return w.buf
}
