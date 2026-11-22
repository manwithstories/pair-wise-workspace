package fixture

import (
	"bytes"
	"encoding/binary"

	gomp4 "github.com/abema/go-mp4"
)

// box wraps payload in a size+type header.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// marshalBox encodes a go-mp4 box (go-mp4 marshals the payload only, so the
// size+type header is added here). Fixtures are built with the same encoder
// the roundtrip check uses, so valid fixtures re-encode byte-identically by
// construction.
func marshalBox(b gomp4.IImmutableBox) []byte {
	var buf bytes.Buffer
	if _, err := gomp4.Marshal(&buf, b, gomp4.Context{}); err != nil {
		panic(err)
	}
	return box(b.GetType().String(), buf.Bytes())
}

func flags3(v uint32) [3]byte {
	return [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

type trackSpec struct {
	id        uint32
	timescale uint32
	defDur    uint32
	defFlags  uint32
}

// buildInit writes an init segment: ftyp + moov(trak..., mvex(trex...)).
func buildInit(tracks []trackSpec) []byte {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso6cmfc"))
	var traks, trexs []byte
	for _, ts := range tracks {
		tkhd := &gomp4.Tkhd{TrackID: ts.id}
		tkhd.Version = 0
		tkhd.Flags = flags3(7) // enabled | in_movie | in_preview
		tkhd.Matrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
		mdhd := &gomp4.Mdhd{Timescale: ts.timescale}
		mdhd.Language = [3]byte{'u' - 0x60, 'n' - 0x60, 'd' - 0x60}
		mdia := box("mdia", marshalBox(mdhd))
		trak := box("trak", append(marshalBox(tkhd), mdia...))
		traks = append(traks, trak...)
		trex := &gomp4.Trex{
			TrackID: ts.id, DefaultSampleDescriptionIndex: 1,
			DefaultSampleDuration: ts.defDur, DefaultSampleFlags: ts.defFlags,
		}
		trexs = append(trexs, marshalBox(trex)...)
	}
	moov := box("moov", append(traks, box("mvex", trexs)...))
	return append(ftyp, moov...)
}

type sampleSpec struct {
	dur   uint32
	size  uint32
	flags uint32
	data  []byte
}

// buildFragment writes one moof+mdat fragment. All trun entries carry
// explicit duration, size and flags.
func buildFragment(seq, trackID uint32, tfdt uint64, samples []sampleSpec) []byte {
	mfhd := &gomp4.Mfhd{SequenceNumber: seq}
	tfhd := &gomp4.Tfhd{TrackID: trackID}
	tfdtBox := &gomp4.Tfdt{BaseMediaDecodeTimeV1: tfdt}
	tfdtBox.Version = 1
	trun := &gomp4.Trun{SampleCount: uint32(len(samples))}
	trun.Flags = flags3(0x100 | 0x200 | 0x400) // duration | size | flags
	var mdat []byte
	for _, s := range samples {
		trun.Entries = append(trun.Entries, gomp4.TrunEntry{
			SampleDuration: s.dur, SampleSize: s.size, SampleFlags: s.flags,
		})
		mdat = append(mdat, s.data...)
	}
	traf := box("traf", append(append(marshalBox(tfhd), marshalBox(tfdtBox)...), marshalBox(trun)...))
	moof := box("moof", append(marshalBox(mfhd), traf...))
	return append(moof, box("mdat", mdat)...)
}
