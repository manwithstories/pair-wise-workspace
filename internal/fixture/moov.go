package fixture

import (
	"io"
)

// moovLayout carries the serialized moov box and the absolute-ish positions
// (relative to moov start) that damage injection may need to patch.
type moovLayout struct {
	data                 []byte
	vSTCOCountPos        int64
	vSTCOPayloadPos      int64
	vSTSZPayloadPos      int64
	aSTCOCountPos        int64
	aSTCOPayloadPos      int64
	aSTSZPayloadPos      int64
}

var identityMatrix = [9]uint32{
	0x00010000, 0, 0,
	0, 0x00010000, 0,
	0, 0, 0x40000000,
}

func writeMatrix(w io.Writer) {
	_, _ = w.Write(identityMatrixBytes())
}

// trackSpec describes one synthetic track while building moov.
type trackSpec struct {
	trackID    uint32
	timescale  uint32
	samples    uint32
	delta      uint32
	startDTS   uint32 // first sample decode time in track timescale
	isVideo    bool
	keyEvery   uint32 // video only
	sizes      []uint32
	handler    string
	language   uint16
}

func writeMvhd(m *memBuffer, timescale, duration uint32) {
	b, _ := BeginFullBox(m, "mvhd", 0, 0)
	var buf [100]byte
	putU32(buf[0:], 0)           // creation
	putU32(buf[4:], 0)           // modification
	putU32(buf[8:], timescale)   // movie timescale
	putU32(buf[12:], duration)   // movie duration
	putU32(buf[16:], 0x00010000) // rate 1.0
	putU16(buf[20:], 0x0100)     // volume 1.0 in 8.8
	// bytes 22..35 reserved
	putU32(buf[36:], 0x00010000) // identity matrix begins
	putU32(buf[56:], 0x00010000)
	putU32(buf[76:], 0x40000000)
	// bytes 84..95 pre-defined
	putU32(buf[96:], 3) // next track ID
	m.Write(buf[:])
	EndBox(m, b)
}

func identityMatrixBytes() []byte {
	out := make([]byte, 36)
	for i, v := range identityMatrix {
		putU32(out[i*4:], v)
	}
	return out
}

func writeTkhd(m *memBuffer, spec trackSpec, movieDuration uint32, movieTS uint32) {
	b, _ := BeginFullBox(m, "tkhd", 0, 0x03) // enabled + in movie
	var head [44]byte
	putU32(head[8:], spec.trackID)
	// track duration is converted to the movie timescale; both tracks use
	// the same 90 kHz timescale in fixtures so the mapping is identity.
	putU32(head[16:], movieDuration)
	putU16(head[36:], 0) // layer
	putU16(head[38:], 0) // alternate group
	if !spec.isVideo {
		putU16(head[40:], 0x0100) // volume
	}
	m.Write(head[:])
	writeMatrix(m)
	var dims [8]byte
	if spec.isVideo {
		putU32(dims[0:], 320<<16)
		putU32(dims[4:], 240<<16)
	}
	m.Write(dims[:])
	EndBox(m, b)
	_ = movieTS
}

func writeMdhd(m *memBuffer, spec trackSpec) {
	b, _ := BeginFullBox(m, "mdhd", 0, 0)
	var head [20]byte
	putU32(head[8:], spec.timescale)
	putU32(head[12:], spec.samples*spec.delta) // media duration
	putU16(head[16:], spec.language)
	m.Write(head[:])
	EndBox(m, b)
}

func writeHdlr(m *memBuffer, handler string) {
	b, _ := BeginFullBox(m, "hdlr", 0, 0)
	var pre [4]byte
	m.Write(pre[:])
	m.Write([]byte(handler))
	var reserved [12]byte
	m.Write(reserved[:])
	m.Write([]byte("mediacheck synthetic\x00"))
	EndBox(m, b)
}

func writeU32(m *memBuffer, v uint32) {
	var b [4]byte
	putU32(b[:], v)
	m.Write(b[:])
}

func writeU16(m *memBuffer, v uint16) {
	var b [2]byte
	putU16(b[:], v)
	m.Write(b[:])
}

// writeStbl emits the sample table and returns patch positions relative
// to the enclosing memBuffer cursor.
func writeStbl(m *memBuffer, spec trackSpec, layout *moovLayout) {
	stbl, _ := BeginBox(m, "stbl")

	// stsd with one minimal sample entry.
	stsd, _ := BeginFullBox(m, "stsd", 0, 0)
	writeU32(m, 1)
	if spec.isVideo {
		writeVideoSampleEntry(m)
	} else {
		writeAudioSampleEntry(m, spec.timescale)
	}
	EndBox(m, stsd)

	stts, _ := BeginFullBox(m, "stts", 0, 0)
	writeU32(m, 1)
	var sttsBody [8]byte
	putU32(sttsBody[0:], spec.samples)
	putU32(sttsBody[4:], spec.delta)
	m.Write(sttsBody[:])
	EndBox(m, stts)

	if spec.isVideo {
		stss, _ := BeginFullBox(m, "stss", 0, 0)
		keys := keyframeNumbers(spec.samples, spec.keyEvery)
		writeU32(m, uint32(len(keys)))
		buf := make([]byte, 4*len(keys))
		for i, k := range keys {
			putU32(buf[i*4:], k)
		}
		m.Write(buf)
		EndBox(m, stss)
	}

	stsc, _ := BeginFullBox(m, "stsc", 0, 0)
	writeU32(m, 1)
	var stscBody [12]byte
	putU32(stscBody[0:], 1) // first chunk
	putU32(stscBody[4:], 1) // one sample per chunk
	putU32(stscBody[8:], 1) // sample description index
	m.Write(stscBody[:])
	EndBox(m, stsc)

	stsz, _ := BeginFullBox(m, "stsz", 0, 0)
	szPos := int64(m.buf.Len())
	var stszHead [12]byte
	putU32(stszHead[4:], spec.samples)
	m.Write(stszHead[:])
	buf := make([]byte, 4*len(spec.sizes))
	for i, s := range spec.sizes {
		putU32(buf[i*4:], s)
	}
	m.Write(buf)
	EndBox(m, stsz)

	stco, _ := BeginFullBox(m, "stco", 0, 0)
	countPos := int64(m.buf.Len())
	writeU32(m, spec.samples)
	payloadPos := int64(m.buf.Len())
	m.Write(make([]byte, 4*int(spec.samples))) // patched after mdat layout
	EndBox(m, stco)

	EndBox(m, stbl)

	if spec.isVideo {
		layout.vSTCOCountPos = countPos
		layout.vSTCOPayloadPos = payloadPos
		layout.vSTSZPayloadPos = szPos + 8 // first entry size position
	} else {
		layout.aSTCOCountPos = countPos
		layout.aSTCOPayloadPos = payloadPos
		layout.aSTSZPayloadPos = szPos + 8
	}
}

func keyframeNumbers(samples, every uint32) []uint32 {
	if every == 0 {
		every = 1
	}
	var keys []uint32
	for k := uint32(1); k <= samples; k += every {
		keys = append(keys, k)
	}
	if len(keys) == 0 || keys[len(keys)-1] != samples {
		// last sample is also marked key so the final segment always has a
		// decodable boundary.
		keys = append(keys, samples)
	}
	return keys
}

// buildMoov serializes a complete moov box into a fresh memory buffer.
func buildMoov(video, audio trackSpec, startDTS uint32, withEdit bool) moovLayout {
	m := &memBuffer{}
	moov, _ := BeginBox(m, "moov")
	movieDur := video.samples*video.delta + startDTS
	writeMvhd(m, video.timescale, movieDur)

	layout := moovLayout{}
	writeTrack(m, video, movieDur, startDTS, withEdit, &layout)
	writeTrack(m, audio, movieDur, 0, withEdit, &layout)

	EndBox(m, moov)
	layout.data = append([]byte(nil), m.buf.Bytes()...)
	return layout
}

func writeTrack(m *memBuffer, spec trackSpec, movieDur, startDTS uint32, withEdit bool, layout *moovLayout) {
	trak, _ := BeginBox(m, "trak")
	writeTkhd(m, spec, movieDur, spec.timescale)
	mdia, _ := BeginBox(m, "mdia")
	writeMdhd(m, spec)
	if spec.isVideo {
		writeHdlr(m, "vide")
	} else {
		writeHdlr(m, "soun")
	}
	minf, _ := BeginBox(m, "minf")
	if spec.isVideo {
		vmhd, _ := BeginFullBox(m, "vmhd", 0, 0x01)
		var vm [8]byte
		m.Write(vm[:])
		EndBox(m, vmhd)
	} else {
		smhd, _ := BeginFullBox(m, "smhd", 0, 0)
		var sm [4]byte
		m.Write(sm[:])
		EndBox(m, smhd)
	}
	writeDinf(m)
	writeStbl(m, spec, layout)
	EndBox(m, minf)

	if withEdit {
		edts, _ := BeginBox(m, "edts")
		elst, _ := BeginFullBox(m, "elst", 0, 0)
		mediaDur := spec.samples * spec.delta
		if startDTS > 0 && spec.isVideo {
			writeU32(m, 2)
			var e0 [12]byte
			putU32(e0[0:], startDTS) // empty edit duration
			putU32(e0[4:], 0xFFFFFFFF)
			putU16(e0[8:], 0x0100)
			m.Write(e0[:])
			var e1 [12]byte
			putU32(e1[0:], mediaDur)
			putU32(e1[4:], startDTS)
			putU16(e1[8:], 0x0100)
			m.Write(e1[:])
		} else {
			writeU32(m, 1)
			var e [12]byte
			putU32(e[0:], mediaDur)
			putU32(e[4:], 0)
			putU16(e[8:], 0x0100)
			m.Write(e[:])
		}
		EndBox(m, elst)
		EndBox(m, edts)
	}
	EndBox(m, mdia)
	EndBox(m, trak)
}

