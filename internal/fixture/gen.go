// Package fixture generates deterministic self-contained MP4 assets for
// tests and the acceptance demo: normal, truncated-mdat, missing-moov
// and corrupt-sample variants. No transcoding is involved; payloads are
// pseudo-random bytes with fixed seed.
package fixture

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	mp4scan "mediaqa/internal/mp4"
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
	out := make([]mp4scan.OutTrack, len(tracks))
	for ti, tr := range tracks {
		out[ti] = mp4scan.OutTrack{ID: tr.ID, Kind: tr.Kind, Timescale: tr.Timescale}
		for si, s := range tr.Samples {
			size := s.Size
			if size == 0 {
				size = 64 + crc32.ChecksumIEEE([]byte(fmt.Sprintf("%d/%d", tr.ID, si)))%256
			}
			out[ti].Samples = append(out[ti].Samples, mp4scan.OutSample{
				Duration: s.Duration,
				Key:      s.Key,
				CTOffset: s.CTOffset,
				Payload:  payload(seed, tr.ID, si, size),
			})
		}
	}
	return mp4scan.MarshalMP4(out)
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
