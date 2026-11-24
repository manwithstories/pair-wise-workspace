// Package fixture generates deterministic local media inputs (MP4 and RTP)
// and controlled corruptions. No external files or network are used.
package fixture

import (
	"bytes"
	"encoding/binary"
	"math/rand"
)

// TrackSpec describes one synthetic track to embed in an MP4 fixture.
type TrackSpec struct {
	ID        uint32
	Video     bool
	Codec     string
	Timescale uint32
	// SampleCount samples are generated; every KeyInterval-th sample is a sync
	// sample. KeyInterval<=1 means every sample is a keyframe.
	Samples      int
	KeyInterval  int
	PayloadBytes int
	// MediaTime sets the elst media_time (non-zero/negative track origin).
	MediaTime int64
	WithElst  bool
}

// box writes an MP4 box: 32-bit size, fourcc, payload.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:8], []byte(typ))
	copy(b[8:], payload)
	return b
}

// fullBox prefixes version+flags onto a payload.
func fullBox(typ string, version uint8, flags uint32, body []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(hdr, body...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// placed records where a track's samples live inside the mdat payload.
type placed struct {
	spec   *TrackSpec
	offset int // offset of first sample within mdat payload
	sizes  []uint32
	keys   []uint32
}

// BuildMP4 assembles a small valid fragmented-free MP4 for the given tracks.
// Sample payloads are deterministic bytes derived from (track, sample).
func BuildMP4(specs []TrackSpec, rng *rand.Rand) []byte {
	var trakBoxes [][]byte
	var mdatPayload []byte
	var placements []placed

	for ti := range specs {
		sp := &specs[ti]
		var sizes []uint32
		var keys []uint32
		offset := len(mdatPayload)
		for s := 0; s < sp.Samples; s++ {
			payload := deterministicPayload(rng, ti, s, sp.PayloadBytes)
			mdatPayload = append(mdatPayload, payload...)
			sizes = append(sizes, uint32(len(payload)))
			if sp.Video {
				if sp.KeyInterval <= 1 || (s%sp.KeyInterval == 0) {
					keys = append(keys, uint32(s+1)) // stss is 1-based
				}
			}
		}
		placements = append(placements, placed{spec: sp, offset: offset, sizes: sizes, keys: keys})
	}

	// Placeholder offsets; real stco values are patched after layout is known.
	ftyp := box("ftyp", concat(
		[]byte("isom"), u32(0x200), []byte("isom"), []byte("iso2"),
	))

	for _, pl := range placements {
		trakBoxes = append(trakBoxes, buildTrak(pl))
	}

	moovInner := bytes.Join(trakBoxes, nil)
	mvhd := buildMvhd(specs)
	moov := box("moov", append(mvhd, moovInner...))
	// mdat must declare its full size (header + payload) in one box.
	mdat := box("mdat", mdatPayload)

	out := append(ftyp, moov...)
	mdatStart := len(out)
	out = append(out, mdat...)

	// Patch stco offsets: each track has exactly one chunk at mdat payload.
	patchStco(out, mdatStart+8, placements)
	return out
}

// buildMvhd creates a minimal movie header box.
func buildMvhd(specs []TrackSpec) []byte {
	var body bytes.Buffer
	body.Write([]byte{0, 0, 0, 0}) // version + flags
	body.Write(u32(0))             // creation
	body.Write(u32(0))             // modification
	body.Write(u32(1000))          // movie timescale
	var maxDur uint32
	for _, sp := range specs {
		d := uint32(sp.Samples) * 1000 / sp.Timescale
		if d > maxDur {
			maxDur = d
		}
	}
	body.Write(u32(maxDur))
	body.Write(u32(0x00010000)) // rate 1.0
	body.Write(u16(0x0100))     // volume
	body.Write(u16(0))          // reserved
	body.Write(make([]byte, 8)) // reserved2
	body.Write(identityMatrix())
	body.Write(make([]byte, 24)) // pre-defined
	var nextID uint32
	for _, sp := range specs {
		if sp.ID > nextID {
			nextID = sp.ID
		}
	}
	body.Write(u32(nextID + 1))
	return box("mvhd", body.Bytes())
}

func identityMatrix() []byte {
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:], 0x00010000)
	binary.BigEndian.PutUint32(m[16:], 0x00010000)
	binary.BigEndian.PutUint32(m[32:], 0x40000000)
	return m
}

// buildTrak assembles one trak box (tkhd, edts optional, mdia).
func buildTrak(pl placed) []byte {
	sp := pl.spec
	var children []byte
	children = append(children, buildTkhd(sp)...)
	if sp.WithElst {
		children = append(children, buildEdts(sp)...)
	}
	children = append(children, buildMdia(pl)...)
	return box("trak", children)
}

func buildTkhd(sp *TrackSpec) []byte {
	var body bytes.Buffer
	body.Write([]byte{0, 0, 0, 3}) // flags = track_enabled|in_movie|in_preview
	body.Write(u32(0))
	body.Write(u32(0))
	body.Write(u32(sp.ID))
	body.Write(u32(0)) // reserved
	dur := uint32(sp.Samples) * 1000 / sp.Timescale
	body.Write(u32(dur))
	body.Write(make([]byte, 8)) // reserved
	body.Write(u16(0))          // layer
	body.Write(u16(0))          // alternate group
	if sp.Video {
		body.Write(u16(0)) // video volume 0
	} else {
		body.Write(u16(0x0100))
	}
	body.Write(u16(0))
	body.Write(identityMatrix())
	if sp.Video {
		body.Write(u32(320 << 16))
		body.Write(u32(240 << 16))
	} else {
		body.Write(u32(0))
		body.Write(u32(0))
	}
	return box("tkhd", body.Bytes())
}

func buildEdts(sp *TrackSpec) []byte {
	var body bytes.Buffer
	body.Write([]byte{0, 0, 0, 0}) // version 0
	body.Write(u32(1))             // entry count
	body.Write(u32(uint32(sp.Samples)))
	if sp.MediaTime >= 0 && sp.MediaTime <= 0x7fffffff {
		body.Write(u32(uint32(sp.MediaTime)))
	} else {
		body.Write(u32(0))
	}
	body.Write(u16(1)) // media rate integer
	body.Write(u16(0)) // fraction
	return box("edts", box("elst", body.Bytes()))
}

func buildMdia(pl placed) []byte {
	sp := pl.spec
	mdhd := buildMdhd(sp)
	hdlr := buildHdlr(sp)
	minf := buildMinf(pl)
	return box("mdia", concat(mdhd, hdlr, minf))
}

func buildMdhd(sp *TrackSpec) []byte {
	var body bytes.Buffer
	body.Write([]byte{0, 0, 0, 0})
	body.Write(u32(0))
	body.Write(u32(0))
	body.Write(u32(sp.Timescale))
	body.Write(u32(uint32(sp.Samples))) // duration in timescale
	body.Write(languageUnd())           // packed ISO-639-2/T language (16 bits)
	body.Write(u16(0))                  // pre-defined
	return box("mdhd", body.Bytes())
}

// languageUnd returns the packed form of "und": 0x55C4 (3 x 5-bit chars,
// each biased by 0x60), occupying exactly two bytes.
func languageUnd() []byte { return u16(0x55c4) }

func buildHdlr(sp *TrackSpec) []byte {
	var body bytes.Buffer
	body.Write([]byte{0, 0, 0, 0})
	body.Write(u32(0)) // pre-defined
	if sp.Video {
		body.Write([]byte("vide"))
	} else {
		body.Write([]byte("soun"))
	}
	body.Write(make([]byte, 12)) // reserved
	body.Write([]byte(sp.Codec))
	body.Write([]byte{0}) // null terminator
	return box("hdlr", body.Bytes())
}

func buildMinf(pl placed) []byte {
	sp := pl.spec
	var mediaHeader []byte
	if sp.Video {
		// vmhd: FullBox(4) + graphicsmode u16 + opcolor[3] u16 = 12 bytes.
		mediaHeader = box("vmhd", concat(
			[]byte{0, 0, 0, 1}, // version + flags(1 = video)
			u16(0), u16(0), u16(0), u16(0)))
	} else {
		// smhd: FullBox(4) + balance u16 + reserved u16 = 8 bytes.
		mediaHeader = box("smhd", concat(
			[]byte{0, 0, 0, 0}, u16(0), u16(0)))
	}
	dinf := box("dinf", box("dref", concat(
		[]byte{0, 0, 0, 0}, u32(1),
		box("url ", []byte{0, 0, 0, 1}), // self-contained flag
	)))
	stbl := buildStbl(pl)
	return box("minf", concat(mediaHeader, dinf, stbl))
}

func buildStbl(pl placed) []byte {
	sp := pl.spec
	stsd := buildStsd(sp)
	stts := buildStts(sp)
	var stss []byte
	if sp.Video && len(pl.keys) > 0 {
		stss = buildStss(pl.keys)
	}
	stsc := fullBox("stsc", 0, 0, concat(u32(1),
		u32(1), u32(uint32(sp.Samples)), u32(1)))
	stsz := buildStsz(pl.sizes)
	// stco placeholder: one chunk, offset patched later. Marker 0xdeadbeef.
	stco := fullBox("stco", 0, 0, concat(u32(1), u32(0xdeadbeef)))
	children := concat(stsd, stts, stsc, stsz, stco)
	if len(stss) > 0 {
		children = concat(stsd, stts, stss, stsc, stsz, stco)
	}
	return box("stbl", children)
}

func buildStsd(sp *TrackSpec) []byte {
	entry := buildSampleEntry(sp)
	body := concat([]byte{0, 0, 0, 0}, u32(1), entry)
	return box("stsd", body)
}

func buildSampleEntry(sp *TrackSpec) []byte {
	var body bytes.Buffer
	body.Write(make([]byte, 6)) // reserved
	body.Write(u16(1))          // data reference index
	if sp.Video {
		body.Write(u16(0))           // pre-defined
		body.Write(u16(0))           // reserved
		body.Write(make([]byte, 12)) // pre-defined[3]
		body.Write(u16(320))
		body.Write(u16(240))
		body.Write(u32(0x00480000)) // horiz resolution 72dpi
		body.Write(u32(0x00480000)) // vert resolution
		body.Write(u32(0))          // reserved
		body.Write(u16(1))          // frame count
		name := make([]byte, 32)
		name[0] = byte(len(sp.Codec))
		copy(name[1:], sp.Codec)
		body.Write(name)
		body.Write(u16(0x0018)) // depth
		body.Write(int16Bytes(-1))
	} else {
		body.Write(u16(0))          // entry version
		body.Write(make([]byte, 6)) // reserved
		body.Write(u16(2))          // channels
		body.Write(u16(16))         // sample size
		body.Write(u16(0))          // pre-defined
		body.Write(u16(0))          // reserved
		body.Write(u32(sp.Timescale << 16))
	}
	return box(sp.Codec, body.Bytes())
}

func buildStts(sp *TrackSpec) []byte {
	// one entry: all samples have delta 1 tick.
	body := concat(u32(1), u32(uint32(sp.Samples)), u32(1))
	return fullBox("stts", 0, 0, body)
}

func buildStss(keys []uint32) []byte {
	body := u32(uint32(len(keys)))
	for _, k := range keys {
		body = append(body, u32(k)...)
	}
	return fullBox("stss", 0, 0, body)
}

func buildStsz(sizes []uint32) []byte {
	body := concat(u32(0), u32(uint32(len(sizes)))) // uniform size 0 -> table
	for _, s := range sizes {
		body = append(body, u32(s)...)
	}
	return fullBox("stsz", 0, 0, body)
}

func int16Bytes(v int16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(v))
	return b
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// deterministicPayload produces reproducible sample bytes from the track and
// sample index only, so repeated runs yield byte-identical fixtures.
func deterministicPayload(_ *rand.Rand, track, sample, n int) []byte {
	b := make([]byte, n)
	local := rand.New(rand.NewSource(int64(track)*1_000_003 + int64(sample)*97 + 1))
	if _, err := local.Read(b); err != nil {
		panic(err) // rand.Rand.Read never errors
	}
	return b
}

// patchStco replaces the placeholder chunk offset in every stco box.
func patchStco(buf []byte, mdatPayloadOffset int, placements []placed) {
	marker := []byte{0xde, 0xad, 0xbe, 0xef}
	pi := 0
	for i := 0; i+4 <= len(buf); i++ {
		if bytes.Equal(buf[i:i+4], marker) {
			binary.BigEndian.PutUint32(buf[i:], uint32(mdatPayloadOffset+placements[pi].offset))
			pi++
		}
	}
}
