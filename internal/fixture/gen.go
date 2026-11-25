// Package fixture generates deterministic self-contained MP4 assets for
// tests and the acceptance demo: normal, truncated-mdat, missing-moov
// and corrupt-sample variants. No transcoding is involved; payloads are
// pseudo-random bytes with fixed seed.
package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/abema/go-mp4"
)

// SampleSpec describes one synthetic sample.
type SampleSpec struct {
	Duration uint32 // ticks
	Key      bool
	CTOffset int32 // composition (PTS-DTS) offset
	Size     uint32 // payload size; 0 picks a deterministic default
}

// TrackSpec describes one synthetic track.
type TrackSpec struct {
	ID        uint32
	Kind      string // "video" or "audio"
	Timescale uint32
	Samples   []SampleSpec
}

// Gen builds a valid MP4 (ftyp, moov, mdat) from the spec. Payload bytes
// are derived from seed so identical specs yield identical files.
func Gen(seed uint32, tracks []TrackSpec) ([]byte, error) {
	// Pass 1: moov with zero offsets to learn its length.
	moov0, err := buildMoov(tracks, nil)
	if err != nil {
		return nil, err
	}
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	mdatHeader := uint64(8)
	dataStart := uint64(len(ftyp)+len(moov0)) + mdatHeader

	// Lay out samples: one chunk per track, samples concatenated.
	offsets := make([][]uint64, len(tracks))
	var mdatPayload bytes.Buffer
	pos := dataStart
	for ti, tr := range tracks {
		offsets[ti] = make([]uint64, len(tr.Samples))
		start := pos
		for si := range tr.Samples {
			offsets[ti][si] = pos
			p := payload(seed, tr.ID, si, sampleSize(tr, si))
			mdatPayload.Write(p)
			pos += uint64(len(p))
		}
		_ = start
	}
	moov, err := buildMoov(tracks, offsets)
	if err != nil {
		return nil, err
	}
	if len(moov) != len(moov0) {
		return nil, fmt.Errorf("moov size changed between passes")
	}
	out := append(ftyp, moov...)
	out = append(out, box("mdat", mdatPayload.Bytes())...)
	return out, nil
}

func sampleSize(tr TrackSpec, i int) uint32 {
	if tr.Samples[i].Size != 0 {
		return tr.Samples[i].Size
	}
	// Deterministic size: 64..319 bytes.
	return 64 + crc32.ChecksumIEEE([]byte(fmt.Sprintf("%d/%d", tr.ID, i)))%256
}

func payload(seed uint32, trackID uint32, idx int, size uint32) []byte {
	p := make([]byte, size)
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], seed)
	binary.BigEndian.PutUint32(b[4:], trackID^uint32(idx))
	v := binary.BigEndian.Uint64(b[:])
	for i := range p {
		v = v*6364136223846793005 + 1442695040888963407
		p[i] = byte(v >> 33)
	}
	return p
}

// box wraps a payload in a 4-byte size + 4-byte type header.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// marshal serializes a leaf box including its size+type header.
// (mp4.Marshal writes only the payload.)
func marshal(b mp4.IImmutableBox) []byte {
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		panic(err)
	}
	typ := b.GetType()
	return box(string(typ[:]), buf.Bytes())
}

func buildMoov(tracks []TrackSpec, offsets [][]uint64) ([]byte, error) {
	var trakBytes []byte
	for ti, tr := range tracks {
		stts := &mp4.Stts{}
		ctts := &mp4.Ctts{}
		stsz := &mp4.Stsz{}
		stss := &mp4.Stss{}
		hasCT := false
		hasNonKey := false
		var dts int64
		_ = dts
		for i, s := range tr.Samples {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
			stsz.EntrySize = append(stsz.EntrySize, sampleSize(tr, i))
			if s.CTOffset != 0 {
				hasCT = true
			}
			ctts.Entries = append(ctts.Entries, mp4.CttsEntry{SampleCount: 1, SampleOffsetV1: s.CTOffset})
			if s.Key {
				stss.SampleNumber = append(stss.SampleNumber, uint32(i+1))
			} else {
				hasNonKey = true
			}
		}
		stts.EntryCount = uint32(len(stts.Entries))
		stsz.SampleCount = uint32(len(tr.Samples))
		ctts.EntryCount = uint32(len(ctts.Entries))
		ctts.Version = 1
		stss.EntryCount = uint32(len(stss.SampleNumber))

		var chunkOff uint32
		if offsets != nil {
			chunkOff = uint32(offsets[ti][0])
		}
		stco := &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{chunkOff}}
		stsc := &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{
			FirstChunk:             1,
			SamplesPerChunk:        uint32(len(tr.Samples)),
			SampleDescriptionIndex: 1,
		}}}

		handler := [4]byte{'v', 'i', 'd', 'e'}
		if tr.Kind == "audio" {
			handler = [4]byte{'s', 'o', 'u', 'n'}
		}
		mdhd := &mp4.Mdhd{Timescale: tr.Timescale}
		hdlr := &mp4.Hdlr{HandlerType: handler, Name: "fixture"}
		tkhd := &mp4.Tkhd{TrackID: tr.ID}

		stbl := marshal(stts)
		if hasCT {
			stbl = append(stbl, marshal(ctts)...)
		}
		stbl = append(stbl, marshal(stsc)...)
		stbl = append(stbl, marshal(stsz)...)
		stbl = append(stbl, marshal(stco)...)
		if hasNonKey {
			stbl = append(stbl, marshal(stss)...)
		}
		minf := box("minf", box("stbl", stbl))
		mdia := box("mdia", append(append(marshal(mdhd), marshal(hdlr)...), minf...))
		trak := box("trak", append(marshal(tkhd), mdia...))
		trakBytes = append(trakBytes, trak...)
	}
	return box("moov", trakBytes), nil
}

// Truncate cuts the file to n bytes, simulating a truncated mdat.
func Truncate(data []byte, n int) []byte {
	if n >= len(data) {
		return data
	}
	out := make([]byte, n)
	copy(out, data[:n])
	return out
}

// StripMoov removes the moov box, simulating a missing index.
func StripMoov(data []byte) []byte {
	var out []byte
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if size < 8 || off+size > len(data) {
			break
		}
		if string(data[off+4:off+8]) != "moov" {
			out = append(out, data[off:off+size]...)
		}
		off += size
	}
	return out
}
