package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// mp4 is a box-tree with explicit sizes so the index package exercises real
// stsc/stco offset math instead of a toy shortcut.
type mp4Box struct {
	typ  string
	body func() []byte
	kids []mp4Box
}

func (b mp4Box) render() []byte {
	var payload []byte
	if b.body != nil {
		payload = b.body()
	}
	for _, k := range b.kids {
		payload = append(payload, k.render()...)
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+len(payload)))
	copy(hdr[4:8], b.typ)
	// FullBox version/flags occupy the first four payload bytes where set.
	return append(hdr[:], payload...)
}

func fullBox(typ string, version uint8, flags uint32, body []byte) mp4Box {
	return mp4Box{typ: typ, body: func() []byte {
		var out [4]byte
		out[0] = version
		out[1] = byte(flags >> 16)
		out[2] = byte(flags >> 8)
		out[3] = byte(flags)
		return append(out[:], body...)
	}}
}

type u32buf struct {
	buf bytes.Buffer
}

func (u *u32buf) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	u.buf.Write(b[:])
}
func (u *u32buf) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	u.buf.Write(b[:])
}
func (u *u32buf) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	u.buf.Write(b[:])
}
func (u *u32buf) bytes(b []byte) { u.buf.Write(b) }

// BuildMP4 writes a minimal ISO-BMFF file with one video track, a fixed
// sample duration and every tenth sample as a sync sample. Two chunks are
// used so the chunk-offset table is non-trivial.
func BuildMP4(path string, samples int, timescale uint32, sampleDur uint32) error {
	if samples <= 0 || timescale == 0 || sampleDur == 0 {
		return fmt.Errorf("invalid mp4 fixture parameters")
	}
	const sampleSize = uint32(64)
	// Sample payloads are plain 64-byte blobs; the indexer treats them as
	// opaque bytes, so their internal format does not matter.
	sampleBytes := make([]byte, int(sampleSize)*samples)
	for i := range sampleBytes {
		sampleBytes[i] = byte(0x40 + i%0x30)
	}

	// Chunk layout: first chunk has n1 samples, second has the rest.
	n1 := samples / 2
	chunk1Offset := uint64(0) // placeholder patched after layout
	var mdatPayload []byte
	mdatPayload = append(mdatPayload, sampleBytes...)

	stts := u32buf{}
	stts.u32(1)
	stts.u32(uint32(samples))
	stts.u32(sampleDur)

	stss := u32buf{}
	var syncs []uint32
	for i := 1; i <= samples; i += 10 {
		syncs = append(syncs, uint32(i))
	}
	stss.u32(uint32(len(syncs)))
	for _, s := range syncs {
		stss.u32(s)
	}

	stsc := u32buf{}
	stsc.u32(2)
	stsc.u32(1) // first chunk
	stsc.u32(uint32(n1))
	stsc.u32(1)
	start2 := uint32(n1 + 1)
	stsc.u32(start2)
	stsc.u32(uint32(samples - n1))
	stsc.u32(1)

	stsz := u32buf{}
	stsz.u32(sampleSize)
	stsz.u32(uint32(samples))

	stco := u32buf{}
	stco.u32(2)
	stco.u32(uint32(chunk1Offset)) // patched below
	stco.u32(uint32(chunk1Offset) + uint32(n1)*sampleSize)

	stbl := mp4Box{typ: "stbl", kids: []mp4Box{
		fullBox("stsd", 0, 0, func() []byte { return nil }()),
		fullBox("stts", 0, 0, stts.buf.Bytes()),
		fullBox("stss", 0, 0, stss.buf.Bytes()),
		fullBox("stsc", 0, 0, stsc.buf.Bytes()),
		fullBox("stsz", 0, 0, stsz.buf.Bytes()),
		fullBox("stco", 0, 0, stco.buf.Bytes()),
	}}
	minf := mp4Box{typ: "minf", kids: []mp4Box{stbl}}
	mdhdBody := u32buf{}
	mdhdBody.u32(0) // creation
	mdhdBody.u32(0) // modification
	mdhdBody.u32(timescale)
	mdhdBody.u32(sampleDur * uint32(samples))
	mdhdBody.u16(0x55c4) // language "und"
	mdhdBody.u16(0)
	mdia := mp4Box{typ: "mdia", kids: []mp4Box{
		fullBox("mdhd", 0, 0, mdhdBody.buf.Bytes()),
		fullBox("hdlr", 0, 0, func() []byte {
			b := u32buf{}
			b.u32(0)
			b.bytes([]byte("vide"))
			b.u32(0)
			b.u32(0)
			b.u32(0)
			b.bytes([]byte("VideoHandler\x00"))
			return b.buf.Bytes()
		}()),
		minf,
	}}
	tkhdBody := u32buf{}
	tkhdBody.u32(0) // creation
	tkhdBody.u32(0) // modification
	tkhdBody.u32(1) // track id
	tkhdBody.u32(0) // reserved
	tkhdBody.u32(sampleDur * uint32(samples))
	tkhdBody.u64(0) // reserved
	tkhdBody.u16(0) // layer
	tkhdBody.u16(0) // alternate group
	tkhdBody.u16(0) // volume
	tkhdBody.u16(0) // reserved
	for i := 0; i < 9; i++ {
		v := uint32(0)
		if i == 0 || i == 4 {
			v = 0x00010000
		}
		if i == 8 {
			v = 0x40000000
		}
		tkhdBody.u32(v)
	}
	tkhdBody.u32(640 << 16)
	tkhdBody.u32(360 << 16)
	trak := mp4Box{typ: "trak", kids: []mp4Box{
		fullBox("tkhd", 0, 3, tkhdBody.buf.Bytes()),
		mdia,
	}}
	mvhdBody := u32buf{}
	mvhdBody.u32(0)
	mvhdBody.u32(0)
	mvhdBody.u32(timescale)
	mvhdBody.u32(sampleDur * uint32(samples))
	mvhdBody.u32(0x00010000)
	mvhdBody.u16(0x0100)
	mvhdBody.u16(0)
	mvhdBody.u64(0)
	mvhdBody.u64(0)
	for i := 0; i < 9; i++ {
		v := uint32(0)
		if i == 0 || i == 4 {
			v = 0x00010000
		}
		if i == 8 {
			v = 0x40000000
		}
		mvhdBody.u32(v)
	}
	for i := 0; i < 6; i++ {
		mvhdBody.u32(0)
	}
	mvhdBody.u32(2)
	moov := mp4Box{typ: "moov", kids: []mp4Box{
		fullBox("mvhd", 0, 0, mvhdBody.buf.Bytes()),
		trak,
	}}
	ftyp := mp4Box{typ: "ftyp", body: func() []byte {
		b := u32buf{}
		b.bytes([]byte("isom"))
		b.u32(512)
		b.bytes([]byte("isomiso2mp41"))
		return b.buf.Bytes()
	}}

	// Patch the chunk offsets now that the ftyp and moov sizes are known.
	// stco lives inside the moov; locate its payload deterministically by
	// rendering the tree once and searching for the "stco" tag.
	moovBytes := moov.render()
	idx := bytes.Index(moovBytes, []byte("stco"))
	if idx < 0 || idx+4+8 > len(moovBytes) {
		return fmt.Errorf("fixture: stco box not found")
	}
	// Payload of stco begins at tag+4: version/flags(4) entryCount(4)
	firstOff := idx + 4 + 8
	if firstOff+8 > len(moovBytes) {
		return fmt.Errorf("fixture: stco payload truncated")
	}
	filePrefix := append(ftyp.render(), moovBytes...)
	mdatOffset := uint64(len(filePrefix)) + 8
	binary.BigEndian.PutUint32(moovBytes[firstOff:firstOff+4], uint32(mdatOffset))
	binary.BigEndian.PutUint32(moovBytes[firstOff+4:firstOff+8], uint32(mdatOffset)+uint32(n1)*sampleSize)

	mdat := mp4Box{typ: "mdat", body: func() []byte { return mdatPayload }}
	var out bytes.Buffer
	out.Write(ftyp.render())
	out.Write(moovBytes)
	out.Write(mdat.render())
	return os.WriteFile(path, out.Bytes(), 0o644)
}
