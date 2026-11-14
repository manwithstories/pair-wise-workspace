// Package fixture generates deterministic MP4 fixtures: normal
// (tail-moov, exact box count/offset), front-moov, edit-list offset,
// 32-bit timestamp wrap and a corrupt file.
package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"mp4tool/internal/box"
)

const (
	movieTimescale = 90000
	mediaTimescale = 90000
	frameDelta     = 7200 // 80ms at 90kHz
)

func be16(v uint16) []byte { var b [2]byte; binary.BigEndian.PutUint16(b[:], v); return b[:] }
func be32(v uint32) []byte { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); return b[:] }
func be64(v uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); return b[:] }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func bx(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func freeBox(size int) []byte { return bx("free", make([]byte, size-8)) }

func ftypBox() []byte {
	return bx("ftyp", concat([]byte("isom"), be32(512), []byte("isom"), []byte("iso2")))
}

var matrix = concat(
	be32(0x00010000), be32(0), be32(0),
	be32(0), be32(0x00010000), be32(0),
	be32(0), be32(0), be32(0x40000000),
)

func mvhdBox(dur, nextID uint32) []byte {
	return bx("mvhd", concat(
		[]byte{0, 0, 0, 0}, be32(0), be32(0), be32(movieTimescale), be32(dur),
		be32(0x00010000), be16(0x0100), be16(0), be32(0), be32(0),
		matrix, make([]byte, 24), be32(nextID),
	))
}

func tkhdBox(id, dur, w, h uint32, volume uint16) []byte {
	return bx("tkhd", concat(
		[]byte{0, 0, 0, 7}, be32(0), be32(0), be32(id), be32(0), be32(dur),
		be32(0), be32(0), be16(0), be16(0), be16(volume), be16(0),
		matrix, be32(w<<16), be32(h<<16),
	))
}

func mdhdBox(dur uint32) []byte {
	return bx("mdhd", concat(
		[]byte{0, 0, 0, 0}, be32(0), be32(0), be32(mediaTimescale), be32(dur),
		be16(0x55c4), be16(0),
	))
}

func hdlrBox(typ, name string) []byte {
	return bx("hdlr", concat(
		be32(0), be32(0), []byte(typ), make([]byte, 12), append([]byte(name), 0),
	))
}

func dinfBox() []byte {
	url := bx("url ", []byte{0, 0, 0, 1})
	return bx("dinf", bx("dref", concat(be32(0), be32(1), url)))
}

func stsdVideoBox() []byte {
	avc1 := bx("avc1", concat(
		make([]byte, 6), be16(1), make([]byte, 16),
		be16(320), be16(240), be32(0x00480000), be32(0x00480000),
		be32(0), be16(1), make([]byte, 32), be16(0x0018), be16(0xffff),
	))
	return bx("stsd", concat(be32(0), be32(1), avc1))
}

func stsdAudioBox() []byte {
	mp4a := bx("mp4a", concat(
		make([]byte, 6), be16(1), make([]byte, 8),
		be16(2), be16(16), be16(0), be16(0), be32(48000<<16),
	))
	return bx("stsd", concat(be32(0), be32(1), mp4a))
}

func sttsBox(entries [][2]uint32) []byte {
	p := concat(be32(0), be32(uint32(len(entries))))
	for _, e := range entries {
		p = concat(p, be32(e[0]), be32(e[1]))
	}
	return bx("stts", p)
}

func ctts0Box(count, offset uint32) []byte {
	return bx("ctts", concat(be32(0), be32(1), be32(count), be32(offset)))
}

func stssBox(nums []uint32) []byte {
	p := concat(be32(0), be32(uint32(len(nums))))
	for _, n := range nums {
		p = concat(p, be32(n))
	}
	return bx("stss", p)
}

func stszBox(sizes []uint32) []byte {
	p := concat(be32(0), be32(0), be32(uint32(len(sizes))))
	for _, s := range sizes {
		p = concat(p, be32(s))
	}
	return bx("stsz", p)
}

func stscBox(perChunk uint32) []byte {
	return bx("stsc", concat(be32(0), be32(1), be32(1), be32(perChunk), be32(1)))
}

func stcoBox(offs []uint64) []byte {
	p := concat(be32(0), be32(uint32(len(offs))))
	for _, o := range offs {
		p = concat(p, be32(uint32(o)))
	}
	return bx("stco", p)
}

type elstEnt struct {
	Dur   uint64
	Media int64
	Rate  uint32 // 16.16
}

func elstBox(version byte, entries []elstEnt) []byte {
	p := concat([]byte{version, 0, 0, 0}, be32(uint32(len(entries))))
	for _, e := range entries {
		if version == 1 {
			p = concat(p, be64(e.Dur), be64(uint64(e.Media)))
		} else {
			p = concat(p, be32(uint32(e.Dur)), be32(uint32(int32(e.Media))))
		}
		p = concat(p, be16(uint16(e.Rate>>16)), be16(uint16(e.Rate&0xffff)))
	}
	return bx("elst", p)
}

// sampleBytes returns deterministic pseudo-content for one sample.
func sampleBytes(idx, size int) []byte {
	b := make([]byte, size)
	for j := range b {
		b[j] = byte((idx*31 + j) % 251)
	}
	return b
}

func sumSizes(sizes []uint32) uint64 {
	var n uint64
	for _, s := range sizes {
		n += uint64(s)
	}
	return n
}

func keyList(n, every int) []uint32 {
	var out []uint32
	for i := 0; i < n; i += every {
		out = append(out, uint32(i+1))
	}
	return out
}

func keySizes(n, every int, key, nonKey uint32) []uint32 {
	sizes := make([]uint32, n)
	for i := range sizes {
		if i%every == 0 {
			sizes[i] = key
		} else {
			sizes[i] = nonKey
		}
	}
	return sizes
}

func minf(header, stbl []byte) []byte { return bx("minf", concat(header, dinfBox(), stbl)) }

func vmhdBox() []byte {
	return bx("vmhd", concat([]byte{0, 0, 0, 1}, be16(0), be16(0), be16(0), be16(0)))
}
func smhdBox() []byte { return bx("smhd", concat(be32(0), be16(0), be16(0))) }

// verify re-parses the generated file with our own box package and
// checks the moov offset and total box count.
func verify(file []byte, wantMoov uint64, wantBoxes int) error {
	r := bytes.NewReader(file)
	moov, total, err := box.FindMoov(r, uint64(len(file)))
	if err != nil {
		return err
	}
	if moov.Offset != wantMoov {
		return fmt.Errorf("moov offset %d, want %d", moov.Offset, wantMoov)
	}
	if total != wantBoxes {
		return fmt.Errorf("box count %d, want %d", total, wantBoxes)
	}
	return nil
}

// SampleContent exposes the deterministic sample generator so the
// selftest can recompute expected hashes independently of offsets.
func SampleContent(idx, size int) []byte { return sampleBytes(idx, size) }
