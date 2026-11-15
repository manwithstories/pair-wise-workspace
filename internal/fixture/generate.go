// Package fixture synthesises deterministic MP4 files offline: normal,
// edit-list, 32-bit timestamp wrap, co64 and moov-at-tail variants.
// Sample payloads are pseudo-random bytes derived from the seed; no
// external media is referenced.
package fixture

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"mp4probe/internal/box"
	"mp4probe/internal/moov"
)

// Cases lists all supported fixture names.
var Cases = []string{"normal", "editlist", "wrap", "co64", "moovtail"}

// Params optionally overrides fixture dimensions (used by tests to
// keep generated files small). Zero fields take case defaults.
type Params struct {
	VideoSamples int
	AudioSamples int
}

// rng is a splitmix64 deterministic generator.
type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// trackSpec describes one synthetic track.
type trackSpec struct {
	id              uint32
	handler         string
	codec           string
	timescale       uint32
	delta           uint32
	count           int
	sizes           []uint32
	syncEvery       int // 0 => no stss (all samples sync)
	bFrames         bool
	samplesPerChunk int
	co64            bool
	stz2            bool
	elst            []moov.ElstEntry
	tkhdDuration    uint64
	mdhdV1          bool
	tkhdV1          bool
}

// fileSpec describes one synthetic file.
type fileSpec struct {
	movieTimescale uint32
	movieDuration  uint64
	mvhdV1         bool
	tracks         []*trackSpec
	moovAtTail     bool
	uuidBox        bool
	unknownBox     bool
}

// Generate writes the requested fixture cases into dir and returns the
// file paths written.
func Generate(dir string, cases []string, seed int64, p Params) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cases {
		spec, err := makeSpec(c, seed, p)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, c+".mp4")
		if err := writeFile(path, spec, seed); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", c, err)
		}
		out = append(out, path)
	}
	return out, nil
}

// makeSizes derives deterministic per-sample sizes.
func makeSizes(seed uint64, count int, base, spread uint32) []uint32 {
	r := &rng{s: seed}
	sizes := make([]uint32, count)
	for i := range sizes {
		sizes[i] = base + uint32(r.next()%uint64(spread))
	}
	return sizes
}

// makeSpec returns the file specification for one case.
func makeSpec(name string, seed int64, p Params) (*fileSpec, error) {
	vcount, acount := p.VideoSamples, p.AudioSamples
	newVideo := func(ts, delta uint32, count int) *trackSpec {
		return &trackSpec{
			id: 1, handler: "vide", codec: "avc1",
			timescale: ts, delta: delta, count: count,
			sizes:           makeSizes(uint64(seed)^0x11, count, 24, 16),
			syncEvery:       20,
			samplesPerChunk: 20,
		}
	}
	newAudio := func(ts, delta uint32, count int) *trackSpec {
		return &trackSpec{
			id: 2, handler: "soun", codec: "mp4a",
			timescale: ts, delta: delta, count: count,
			sizes:           makeSizes(uint64(seed)^0x22, count, 12, 8),
			samplesPerChunk: 25,
		}
	}
	switch name {
	case "normal", "co64", "moovtail":
		if vcount == 0 {
			vcount = 750
		}
		if acount == 0 {
			acount = 704
		}
		v := newVideo(90000, 3600, vcount)
		v.syncEvery = 10
		v.samplesPerChunk = 5
		v.bFrames = true
		v.tkhdDuration = uint64(vcount) * 3600
		a := newAudio(48000, 2048, acount)
		a.samplesPerChunk = 20
		a.tkhdDuration = uint64(acount) * 2048 * 90000 / 48000
		spec := &fileSpec{
			movieTimescale: 90000,
			movieDuration:  uint64(vcount) * 3600,
			tracks:         []*trackSpec{v, a},
		}
		switch name {
		case "normal":
			a.stz2 = true
			spec.unknownBox = true
		case "co64":
			v.co64 = true
		case "moovtail":
			spec.moovAtTail = true
			spec.uuidBox = true
		}
		return spec, nil
	case "editlist":
		if vcount == 0 {
			vcount = 12000
		}
		if acount == 0 {
			acount = 11600
		}
		v := newVideo(90000, 3600, vcount)
		// Empty edit, then a segment starting 0.16s into the media,
		// then a half-rate tail. Total elst = 44610000; tkhd is set
		// to 44650000 on purpose to record a conflict.
		v.elst = []moov.ElstEntry{
			{Duration: 450000, MediaTime: -1, Rate: 1 << 16},
			{Duration: 40500000, MediaTime: 14400, Rate: 1 << 16},
			{Duration: 3600000, MediaTime: 40514400, Rate: 1 << 15},
		}
		v.tkhdDuration = 44650000
		a := newAudio(48000, 2048, acount)
		a.samplesPerChunk = 20
		a.tkhdDuration = uint64(acount) * 2048 * 90000 / 48000
		return &fileSpec{
			movieTimescale: 90000,
			movieDuration:  44650000,
			tracks:         []*trackSpec{v, a},
		}, nil
	case "wrap":
		if vcount == 0 {
			vcount = 1200000
		}
		if acount == 0 {
			acount = 1125000
		}
		v := newVideo(90000, 3600, vcount)
		v.mdhdV1, v.tkhdV1 = true, true
		v.tkhdDuration = 1200125000
		v.elst = []moov.ElstEntry{
			{Duration: 125000, MediaTime: -1, Rate: 1 << 16},
			{Duration: 600000000, MediaTime: 0, Rate: 1 << 16},
			{Duration: 600000000, MediaTime: 2160000000, Rate: 1 << 16},
		}
		a := newAudio(48000, 2048, acount)
		a.tkhdDuration = 1200000000
		return &fileSpec{
			movieTimescale: 25000,
			movieDuration:  1200125000,
			tracks:         []*trackSpec{v, a},
		}, nil
	}
	return nil, fmt.Errorf("unknown fixture case %q", name)
}

// be32 appends a big-endian uint32.
func be32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

// be64 appends a big-endian uint64.
func be64(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

// serializeMvhd builds the movie header.
func serializeMvhd(spec *fileSpec) []byte {
	var body []byte
	if spec.mvhdV1 {
		body = be64(body, 0)
		body = be64(body, 0)
		body = be32(body, spec.movieTimescale)
		body = be64(body, spec.movieDuration)
	} else {
		body = be32(body, 0)
		body = be32(body, 0)
		body = be32(body, spec.movieTimescale)
		body = be32(body, uint32(spec.movieDuration))
	}
	body = be32(body, 0x00010000)   // rate 1.0
	body = append(body, 0x01, 0x00) // volume 1.0
	body = append(body, make([]byte, 10)...)
	body = append(body, unityMatrix()...)
	body = append(body, make([]byte, 24)...)
	body = be32(body, uint32(len(spec.tracks))+1)
	version := byte(0)
	if spec.mvhdV1 {
		version = 1
	}
	return box.PackFull("mvhd", version, 0, body)
}

func unityMatrix() []byte {
	return []byte{
		0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0, 0, 0,
	}
}

// serializeTkhd builds the track header.
func serializeTkhd(t *trackSpec) []byte {
	var body []byte
	if t.tkhdV1 {
		body = be64(body, 0)
		body = be64(body, 0)
		body = be32(body, t.id)
		body = be32(body, 0)
		body = be64(body, t.tkhdDuration)
	} else {
		body = be32(body, 0)
		body = be32(body, 0)
		body = be32(body, t.id)
		body = be32(body, 0)
		body = be32(body, uint32(t.tkhdDuration))
	}
	body = append(body, make([]byte, 8)...) // reserved
	body = append(body, 0, 0, 0, 0)         // layer, alternate group
	if t.handler == "soun" {
		body = append(body, 0x01, 0x00) // volume
	} else {
		body = append(body, 0, 0)
	}
	body = append(body, 0, 0) // reserved
	body = append(body, unityMatrix()...)
	if t.handler == "vide" {
		body = be32(body, 1920<<16)
		body = be32(body, 1080<<16)
	} else {
		body = be32(body, 0)
		body = be32(body, 0)
	}
	version := byte(0)
	if t.tkhdV1 {
		version = 1
	}
	return box.PackFull("tkhd", version, 7, body)
}

// serializeMdhd builds the media header.
func serializeMdhd(t *trackSpec) []byte {
	duration := uint64(t.count) * uint64(t.delta)
	var body []byte
	if t.mdhdV1 {
		body = be64(body, 0)
		body = be64(body, 0)
		body = be32(body, t.timescale)
		body = be64(body, duration)
	} else {
		body = be32(body, 0)
		body = be32(body, 0)
		body = be32(body, t.timescale)
		body = be32(body, uint32(duration))
	}
	body = append(body, 0x55, 0xc4, 0, 0) // language "und", pre_defined
	version := byte(0)
	if t.mdhdV1 {
		version = 1
	}
	return box.PackFull("mdhd", version, 0, body)
}

// serializeElst builds a version-1 edit list.
func serializeElst(elst []moov.ElstEntry) []byte {
	var body []byte
	body = be32(body, uint32(len(elst)))
	for _, e := range elst {
		body = be64(body, uint64(e.Duration))
		body = be64(body, uint64(e.MediaTime))
		body = append(body, byte(e.Rate>>24), byte(e.Rate>>16))
		body = append(body, byte(e.Rate>>8), byte(e.Rate))
	}
	return box.PackFull("elst", 1, 0, body)
}

// serializeStbl builds the sample table box. chunkOffsets must already
// be final; two passes keep moov size stable while offsets are fixed up.
func serializeStbl(t *trackSpec, chunkOffsets []uint64) []byte {
	// stsd
	var entry []byte
	entry = be32(entry, 0) // size placeholder
	entry = append(entry, t.codec...)
	entry = append(entry, make([]byte, 6)...)
	entry = be32(entry, 0) // data_reference_index(2)+padding -> fix below
	binary.BigEndian.PutUint16(entry[12:], 1)
	binary.BigEndian.PutUint32(entry[0:], uint32(len(entry)))
	stsd := box.PackFull("stsd", 0, 0, append(be32(nil, 1), entry...))
	// stts
	stts := box.PackFull("stts", 0, 0,
		be32(be32(be32(nil, 1), uint32(t.count)), t.delta))
	// ctts (version 1, B-frame reorder pattern 0,+1,-1 frames)
	var ctts []byte
	if t.bFrames {
		body := be32(nil, uint32(t.count))
		for i := 0; i < t.count; i++ {
			body = be32(body, 1)
			switch {
			case i == 0:
				body = be32(body, 0)
			case i%2 == 1:
				body = be32(body, uint32(t.delta))
			default:
				body = be32(body, uint32(0)-t.delta)
			}
		}
		ctts = box.PackFull("ctts", 1, 0, body)
	}
	// stss
	var stss []byte
	if t.syncEvery > 0 {
		var body []byte
		n := (t.count + t.syncEvery - 1) / t.syncEvery
		body = be32(body, uint32(n))
		for i := 0; i < t.count; i += t.syncEvery {
			body = be32(body, uint32(i)+1)
		}
		stss = box.PackFull("stss", 0, 0, body)
	}
	// stsc: single run
	stsc := box.PackFull("stsc", 0, 0,
		be32(be32(be32(be32(nil, 1), 1), uint32(t.samplesPerChunk)), 1))
	// stsz or stz2
	var sztab []byte
	if t.stz2 {
		body := append([]byte{0, 0, 0, 8}, be32(nil, uint32(t.count))...)
		for _, s := range t.sizes {
			body = append(body, byte(s))
		}
		sztab = box.PackFull("stz2", 0, 0, body)
	} else {
		body := be32(be32(nil, 0), uint32(t.count))
		for _, s := range t.sizes {
			body = be32(body, s)
		}
		sztab = box.PackFull("stsz", 0, 0, body)
	}
	// stco or co64
	var cotab []byte
	body := be32(nil, uint32(len(chunkOffsets)))
	if t.co64 {
		for _, o := range chunkOffsets {
			body = be64(body, o)
		}
		cotab = box.PackFull("co64", 0, 0, body)
	} else {
		for _, o := range chunkOffsets {
			body = be32(body, uint32(o))
		}
		cotab = box.PackFull("stco", 0, 0, body)
	}
	var stbl []byte
	stbl = append(stbl, stsd...)
	stbl = append(stbl, stts...)
	stbl = append(stbl, ctts...)
	stbl = append(stbl, stss...)
	stbl = append(stbl, stsc...)
	stbl = append(stbl, sztab...)
	stbl = append(stbl, cotab...)
	return box.Pack("stbl", stbl)
}

// serializeHdlr builds the handler box.
func serializeHdlr(t *trackSpec) []byte {
	var body []byte
	body = be32(body, 0)
	body = append(body, t.handler...)
	body = append(body, make([]byte, 12)...)
	body = append(body, 0) // empty name
	return box.PackFull("hdlr", 0, 0, body)
}

// serializeTrak assembles one trak box.
func serializeTrak(t *trackSpec, chunkOffsets []uint64) []byte {
	var mdia []byte
	mdia = append(mdia, serializeMdhd(t)...)
	mdia = append(mdia, serializeHdlr(t)...)
	var mediaHeader []byte
	if t.handler == "vide" {
		mediaHeader = box.PackFull("vmhd", 0, 1, make([]byte, 8))
	} else {
		mediaHeader = box.PackFull("smhd", 0, 0, make([]byte, 4))
	}
	dref := box.PackFull("dref", 0, 0,
		append(be32(nil, 1), box.PackFull("url ", 0, 1, nil)...))
	minf := append(mediaHeader, box.Pack("dinf", dref)...)
	minf = append(minf, serializeStbl(t, chunkOffsets)...)
	mdia = append(mdia, box.Pack("minf", minf)...)
	var trak []byte
	trak = append(trak, serializeTkhd(t)...)
	if len(t.elst) > 0 {
		trak = append(trak, box.Pack("edts", serializeElst(t.elst))...)
	}
	trak = append(trak, box.Pack("mdia", mdia)...)
	return box.Pack("trak", trak)
}

// serializeMoov assembles the whole moov box.
func serializeMoov(spec *fileSpec, offsets map[uint32][]uint64) []byte {
	var moov []byte
	moov = append(moov, serializeMvhd(spec)...)
	for _, t := range spec.tracks {
		moov = append(moov, serializeTrak(t, offsets[t.id])...)
	}
	return box.Pack("moov", moov)
}

// chunkLayout computes chunk sizes (in samples) for a track.
func chunkLayout(t *trackSpec) []int {
	var chunks []int
	remaining := t.count
	for remaining > 0 {
		n := t.samplesPerChunk
		if n > remaining {
			n = remaining
		}
		chunks = append(chunks, n)
		remaining -= n
	}
	return chunks
}

// writeSamplePayload streams one deterministic pseudo-random sample.
func writeSamplePayload(w *bufio.Writer, seed uint64, trackID uint32, sampleIdx int, size uint32, scratch *[]byte) {
	r := &rng{s: seed ^ uint64(trackID)<<40 ^ uint64(sampleIdx)*0x2545F4914F6CDD1D}
	if cap(*scratch) < int(size) {
		*scratch = make([]byte, size)
	}
	buf := (*scratch)[:size]
	for i := 0; i+8 <= len(buf); i += 8 {
		binary.BigEndian.PutUint64(buf[i:], r.next())
	}
	if rem := len(buf) % 8; rem > 0 {
		tail := r.next()
		for i := 0; i < rem; i++ {
			buf[len(buf)-rem+i] = byte(tail >> (8 * i))
		}
	}
	w.Write(buf)
}

// writeFile renders the fixture to disk. moov size is computed with
// placeholder offsets first, then chunk offsets are fixed and moov is
// re-serialised at the identical size.
func writeFile(path string, spec *fileSpec, seed int64) error {
	ftyp := box.Pack("ftyp", append([]byte("isom"), be32(append([]byte("isom"), 0, 0, 2, 0), 0)...))
	var extras []byte
	if spec.uuidBox {
		extras = append(extras, box.PackUUID([16]byte{0x6d, 0x70, 0x34, 0x70}, []byte("mp4probe-fixture"))...)
	}
	if spec.unknownBox {
		extras = append(extras, box.Pack("zzzz", []byte("ignored"))...)
	}
	// Pass 1: placeholder offsets to measure moov.
	layouts := make(map[uint32][]int)
	offsets := make(map[uint32][]uint64)
	for _, t := range spec.tracks {
		layouts[t.id] = chunkLayout(t)
		offsets[t.id] = make([]uint64, len(layouts[t.id]))
	}
	moovLen := uint64(len(serializeMoov(spec, offsets)))
	// Compute mdat placement.
	headLen := uint64(len(ftyp) + len(extras))
	var mdatDataStart uint64
	if spec.moovAtTail {
		mdatDataStart = headLen + 8
	} else {
		mdatDataStart = headLen + moovLen + 8
	}
	// Assign chunk offsets sequentially in track order.
	cursor := mdatDataStart
	for _, t := range spec.tracks {
		sample := 0
		for ci, n := range layouts[t.id] {
			offsets[t.id][ci] = cursor
			for s := 0; s < n; s++ {
				cursor += uint64(t.sizes[sample])
				sample++
			}
		}
	}
	mdatPayload := cursor - mdatDataStart
	moovBytes := serializeMoov(spec, offsets)
	if uint64(len(moovBytes)) != moovLen {
		return fmt.Errorf("moov size changed between passes: %d != %d", len(moovBytes), moovLen)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.Write(ftyp); err != nil {
		return err
	}
	if _, err := w.Write(extras); err != nil {
		return err
	}
	writeMdat := func() error {
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[0:4], uint32(mdatPayload)+8)
		copy(hdr[4:8], "mdat")
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		var scratch []byte
		for _, t := range spec.tracks {
			for i := 0; i < t.count; i++ {
				writeSamplePayload(w, uint64(seed), t.id, i, t.sizes[i], &scratch)
			}
		}
		return nil
	}
	if spec.moovAtTail {
		if err := writeMdat(); err != nil {
			return err
		}
		if _, err := w.Write(moovBytes); err != nil {
			return err
		}
	} else {
		if _, err := w.Write(moovBytes); err != nil {
			return err
		}
		if err := writeMdat(); err != nil {
			return err
		}
	}
	return w.Flush()
}
