// Package fixture synthesizes deterministic MP4/MOV sample files used by
// the tests and the demo: valid multi-bitrate material plus truncated,
// index-less and corrupted variants. No external media files are needed.
package fixture

import (
	"bytes"
	"encoding/binary"

	mp4 "github.com/abema/go-mp4"
)

// sampleSpec describes one synthesized sample.
type sampleSpec struct {
	delta    uint32 // decoding duration in track timescale
	size     uint32
	keyframe bool
	cto      int32 // composition time offset (ctts v1)
	data     []byte
}

// trackSpec describes one synthesized track.
type trackSpec struct {
	id        uint32
	kind      string // "video" or "audio"
	timescale uint32
	width     uint32
	height    uint32
	samples   []sampleSpec
}

func boxOf(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// leaf marshals a go-mp4 box payload (without the box header).
func leaf(b mp4.IImmutableBox) []byte {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// leafBox marshals a complete box (header + payload).
func leafBox(b mp4.IImmutableBox) []byte {
	var typ [4]byte
	copy(typ[:], b.GetType().String())
	return boxOf(string(typ[:]), leaf(b))
}

var identityMatrix = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

// buildMP4 assembles a faststart-style MP4: ftyp, moov, mdat. Chunk
// offsets are computed in two passes because the moov size is needed to
// locate the mdat payload. When omitStss is set the keyframe index is
// dropped to simulate a missing sample table.
func buildMP4(movieTimescale uint32, tracks []trackSpec, omitStss bool) []byte {
	ftyp := leafBox(&mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', '2'}},
			{CompatibleBrand: [4]byte{'a', 'v', 'c', '1'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	})
	var mdatPayload []byte
	for _, tr := range tracks {
		for _, s := range tr.samples {
			mdatPayload = append(mdatPayload, s.data...)
		}
	}
	zeros := make([]uint64, len(tracks))
	moovLen := len(buildMoov(movieTimescale, tracks, zeros, omitStss))
	offsets := make([]uint64, len(tracks))
	pos := uint64(len(ftyp) + moovLen + 8) // 8 = mdat header
	for i, tr := range tracks {
		offsets[i] = pos
		for _, s := range tr.samples {
			pos += uint64(s.size)
		}
	}
	moov := buildMoov(movieTimescale, tracks, offsets, omitStss)
	out := append(ftyp, moov...)
	return append(out, boxOf("mdat", mdatPayload)...)
}

func buildMoov(movieTimescale uint32, tracks []trackSpec, chunkOffsets []uint64, omitStss bool) []byte {
	var movieDur uint32
	var maxTrackID uint32
	var traks []byte
	for i, tr := range tracks {
		dur := trackDuration(tr)
		md := uint32(uint64(dur) * uint64(movieTimescale) / uint64(tr.timescale))
		if md > movieDur {
			movieDur = md
		}
		if tr.id > maxTrackID {
			maxTrackID = tr.id
		}
		traks = append(traks, buildTrak(tr, md, chunkOffsets[i], omitStss)...)
	}
	mvhd := leafBox(&mp4.Mvhd{
		Timescale:   movieTimescale,
		DurationV0:  movieDur,
		Rate:        0x10000,
		Volume:      0x0100,
		Matrix:      identityMatrix,
		NextTrackID: maxTrackID + 1,
	})
	return boxOf("moov", append(mvhd, traks...))
}

func trackDuration(tr trackSpec) uint64 {
	var d uint64
	for _, s := range tr.samples {
		d += uint64(s.delta)
	}
	return d
}

func buildTrak(tr trackSpec, movieDur uint32, chunkOffset uint64, omitStss bool) []byte {
	tkhd := &mp4.Tkhd{
		FullBox:    mp4.FullBox{Flags: [3]byte{0, 0, 7}},
		TrackID:    tr.id,
		DurationV0: movieDur,
		Matrix:     identityMatrix,
	}
	if tr.kind == "video" {
		tkhd.Width = tr.width << 16
		tkhd.Height = tr.height << 16
	} else {
		tkhd.Volume = 0x0100
	}
	return boxOf("trak", append(leafBox(tkhd), buildMdia(tr, chunkOffset, omitStss)...))
}

func buildMdia(tr trackSpec, chunkOffset uint64, omitStss bool) []byte {
	mdhd := leafBox(&mp4.Mdhd{
		Timescale:  tr.timescale,
		DurationV0: uint32(trackDuration(tr)),
		Language:   [3]byte{'u', 'n', 'd'},
	})
	handler := [4]byte{'s', 'o', 'u', 'n'}
	name := "SoundHandler"
	if tr.kind == "video" {
		handler = [4]byte{'v', 'i', 'd', 'e'}
		name = "VideoHandler"
	}
	hdlr := leafBox(&mp4.Hdlr{HandlerType: handler, Name: name})
	return boxOf("mdia", concat(mdhd, hdlr, buildMinf(tr, chunkOffset, omitStss)))
}

func buildMinf(tr trackSpec, chunkOffset uint64, omitStss bool) []byte {
	var mediaHeader []byte
	if tr.kind == "video" {
		mediaHeader = leafBox(&mp4.Vmhd{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}})
	} else {
		mediaHeader = leafBox(&mp4.Smhd{})
	}
	url := leafBox(&mp4.Url{FullBox: mp4.FullBox{Flags: [3]byte{0, 0, 1}}})
	dref := boxOf("dref", append(leaf(&mp4.Dref{EntryCount: 1}), url...))
	dinf := boxOf("dinf", dref)
	return boxOf("minf", concat(mediaHeader, dinf, buildStbl(tr, chunkOffset, omitStss)))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
