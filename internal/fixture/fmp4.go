package fixture

import (
	"bytes"
	"encoding/binary"
)

// ftyp+moov initialization segment. The boxes carry just enough structural
// information for go-mp4 to recognize a valid fMP4 init; sample generation is
// fully deterministic and independent of any external asset.
func buildInitSegment(spec Spec) []byte {
	ftyp := boxBytes("ftyp", concat(
		[]byte("iso5"),            // major brand
		u32(1),                    // minor version
		[]byte("iso5"), []byte("dash"), // compatible brands
	))
	mvhd := boxBytes("mvhd", fullBox(0, concat(
		u32(0), u32(0), u32(Timescale), u32(SegmentCount*1000),
		bytes.Repeat([]byte{0}, 80), // rate/volume/matrix/predefined
		u32(2), // next track id
	)))
	trak := buildTrak(spec)
	moov := boxBytes("moov", concat(mvhd, trak))
	return concat(ftyp, moov)
}

func buildTrak(spec Spec) []byte {
	tkhd := boxBytes("tkhd", fullBox(0x000007, concat(
		u32(0), u32(0), u32(1), u32(0), u32(SegmentCount*1000),
		bytes.Repeat([]byte{0}, 16), u16(0), u16(0), u16(0), u16(0),
		u32(fixed16(1)), u32(fixed16(1)),
		u32(0), u32(0), u32(fixed16(1)), u32(0),
		bytes.Repeat([]byte{0}, 24), u16(uint16(spec.Width)), u16(0),
		u16(uint16(spec.Height)), u16(0),
	)))
	mdhd := boxBytes("mdhd", fullBox(0, concat(
		u32(0), u32(0), u32(Timescale), u32(SegmentCount*1000),
		u16(0x55c4), u16(0), // language 'und' + pre-defined
	)))
	hdlr := boxBytes("hdlr", fullBox(0, concat(
		u32(0), []byte("vide"), bytes.Repeat([]byte{0}, 12),
		[]byte("VideoHandler\x00"),
	)))
	codec := []byte("avc1")
	if isHEVC(spec.Codec) {
		codec = []byte("hvc1")
	}
	videoEntry := boxBytes(string(codec), concat(
		bytes.Repeat([]byte{0}, 6), u16(1),
		u16(0), u16(0), bytes.Repeat([]byte{0}, 16),
		u16(uint16(spec.Width)), u16(uint16(spec.Height)),
		u32(0x00480000), u32(0x00480000), u32(0),
		u16(1), bytes.Repeat([]byte{0}, 32), u16(0x0018), u16(0xffff),
	))
	dinf := boxBytes("dinf", boxBytes("dref", fullBox(0, concat(u32(1),
		boxBytes("url ", fullBox(0x000001, nil))))))
	stbl := boxBytes("stbl", concat(
		boxBytes("stsd", fullBox(0, concat(u32(1), videoEntry))),
		boxBytes("stts", fullBox(0, concat(u32(0), u32(0)))),
		boxBytes("stsc", fullBox(0, u32(0))),
		boxBytes("stsz", fullBox(0, concat(u32(0), u32(0)))),
		boxBytes("stco", fullBox(0, u32(0))),
	))
	minf := boxBytes("minf", concat(
		boxBytes("vmhd", fullBox(0x000001, u16(0))),
		dinf, stbl,
	))
	mdia := boxBytes("mdia", concat(mdhd, hdlr, minf))
	return boxBytes("trak", concat(tkhd, mdia))
}

// mediaFragment builds one moof+mdat fragment with deterministic samples. The
// first sample of each fragment is an IDR/CRA keyframe, producing a fixed GOP.
func mediaFragment(spec Spec, sequence int, baseDecode uint64) []byte {
	var entries []sampleEntry
	var mdat bytes.Buffer
	for s := 0; s < SamplesPerFragment; s++ {
		keyframe := s == 0
		nal := deterministicNAL(spec, sequence, s, keyframe)
		prefixed := concat(u32(uint32(len(nal))), nal)
		entries = append(entries, sampleEntry{
			duration: SampleDuration, size: uint32(len(prefixed)),
		})
		mdat.Write(prefixed)
	}
	moof := buildMoof(1, sequence, baseDecode, entries)
	return concat(moof, boxBytes("mdat", mdat.Bytes()))
}

type sampleEntry struct {
	duration uint32
	size     uint32
}

func buildMoof(trackID, sequence int, baseDecode uint64, entries []sampleEntry) []byte {
	mfhd := boxBytes("mfhd", fullBox(0, u32(uint32(sequence))))
	tfhd := boxBytes("tfhd", fullBox(0, u32(uint32(trackID))))
	tfdt := boxBytes("tfdt", fullBox(0x01000000, u64(baseDecode)))
	var trunBody bytes.Buffer
	trunBody.Write(u32(uint32(len(entries))))
	for _, e := range entries {
		trunBody.Write(u32(e.duration))
		trunBody.Write(u32(e.size))
	}
	trun := boxBytes("trun", fullBox(0x000300, trunBody.Bytes()))
	return boxBytes("moof", concat(mfhd, tfhd, tfdt, trun))
}

// deterministicNAL produces a small, repeatable NAL unit. Payload bytes derive
// only from the coordinates, so the same fragment always hashes identically.
func deterministicNAL(spec Spec, sequence, sample int, keyframe bool) []byte {
	var header byte
	if isHEVC(spec.Codec) {
		nalType := byte(1) // TRAIL_N
		if keyframe {
			nalType = 19 // IDR_W_RADL
		}
		header = nalType << 1
	} else {
		if keyframe {
			header = 5 // IDR
		} else {
			header = 1 // non-IDR slice
		}
	}
	payload := []byte{
		byte(sequence >> 8), byte(sequence),
		byte(sample), byte(spec.BPS >> 16), byte(spec.BPS >> 8), byte(spec.BPS),
	}
	return append([]byte{header}, payload...)
}

func isHEVC(codec string) bool {
	return len(codec) >= 4 && (codec[:4] == "hvc1" || codec[:4] == "hev1")
}

func boxBytes(fourcc string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], fourcc)
	copy(out[8:], payload)
	return out
}

func fullBox(flags uint32, body []byte) []byte {
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[0:4], flags)
	copy(out[4:], body)
	return out
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

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func fixed16(v float64) uint32 { return uint32(v *65536) }
