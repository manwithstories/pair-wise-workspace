package mp4scan

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"
)

// OutSample is one sample to write into a remuxed MP4.
type OutSample struct {
	Duration uint32
	Key      bool
	CTOffset int32 // composition offset (PTS-DTS), may be negative
	Payload  []byte
}

// OutTrack is one track of a remuxed MP4.
type OutTrack struct {
	ID        uint32
	Kind      string // "video" or "audio"
	Timescale uint32
	Samples   []OutSample
}

// MarshalMP4 builds a complete MP4 (ftyp, moov, mdat) with one chunk per
// track. Output is deterministic for identical input.
func MarshalMP4(tracks []OutTrack) ([]byte, error) {
	moov0, err := buildMoov(tracks, nil)
	if err != nil {
		return nil, err
	}
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	pos := uint64(len(ftyp)+len(moov0)) + 8 // + mdat header

	offsets := make([][]uint64, len(tracks))
	var mdatPayload bytes.Buffer
	for ti, tr := range tracks {
		offsets[ti] = make([]uint64, len(tr.Samples))
		for si, s := range tr.Samples {
			offsets[ti][si] = pos
			mdatPayload.Write(s.Payload)
			pos += uint64(len(s.Payload))
		}
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

func buildMoov(tracks []OutTrack, offsets [][]uint64) ([]byte, error) {
	var trakBytes []byte
	for ti, tr := range tracks {
		stts := &mp4.Stts{}
		ctts := &mp4.Ctts{}
		ctts.Version = 1 // signed composition offsets
		stsz := &mp4.Stsz{}
		stss := &mp4.Stss{}
		hasCT, hasNonKey := false, false
		for si, s := range tr.Samples {
			stts.Entries = append(stts.Entries, mp4.SttsEntry{SampleCount: 1, SampleDelta: s.Duration})
			stsz.EntrySize = append(stsz.EntrySize, uint32(len(s.Payload)))
			if s.CTOffset != 0 {
				hasCT = true
			}
			ctts.Entries = append(ctts.Entries, mp4.CttsEntry{SampleCount: 1, SampleOffsetV1: s.CTOffset})
			if s.Key {
				stss.SampleNumber = append(stss.SampleNumber, uint32(si+1))
			} else {
				hasNonKey = true
			}
		}
		stts.EntryCount = uint32(len(stts.Entries))
		stsz.SampleCount = uint32(len(tr.Samples))
		ctts.EntryCount = uint32(len(ctts.Entries))
		stss.EntryCount = uint32(len(stss.SampleNumber))

		var chunkOff uint32
		if offsets != nil && len(tr.Samples) > 0 {
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
		hdlr := &mp4.Hdlr{HandlerType: handler, Name: "mediaqa"}
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
