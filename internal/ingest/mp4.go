package ingest

import (
	"bytes"
	"encoding/binary"
	"path/filepath"

	"archive/internal/media"
)

// Crop is a clean-aperture rectangle in source pixels.
type Crop struct {
	X, Y, W, H int
}

// ColorDesc is a color description with its provenance.
type ColorDesc struct {
	Primaries, Transfer, Matrix byte
	Source                    string
}

// ParityInfo carries everything needed to rebuild one lost packet in a
// group of four: per-packet headers plus the XOR of all payloads.
type ParityInfo struct {
	BaseSeq int64
	Count   int
	PTS     []int64 // local-clock nanoseconds per packet
	Samples []int
	Lengths []int
	XOR     []byte
}

// MP4Result is the parsed content of one MP4 source.
type MP4Result struct {
	Matrix    [9]int32 // tkhd display matrix, 16.16 fixed point
	HasMatrix bool
	Crop      *Crop
	Color     *ColorDesc
	Anchors   []media.Anchor
	Packets   []*media.Packet // audio packets, local-clock PTS
	Parity    []ParityInfo
	Faults    []*media.Fault
}

// ParseMP4 parses the supported box subset. Unknown box types and
// conflicting same-level color boxes are fatal; a truncated trailing box
// is recorded in Faults with track, stage and byte offset.
func ParseMP4(path string, track int) (*MP4Result, error) {
	data, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	res := &MP4Result{}
	src := filepath.Base(path)
	off := 0
	for off < len(data) {
		if len(data)-off < 8 {
			res.Faults = append(res.Faults, media.Faultf(media.StageParse, track,
				int64(off), "truncated box header: %d bytes remain", len(data)-off))
			break
		}
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		if size < 8 || off+size > len(data) {
			res.Faults = append(res.Faults, media.Faultf(media.StageParse, track,
				int64(off), "truncated box %q: declared %d bytes, %d remain",
				typ, size, len(data)-off))
			break
		}
		body := data[off+8 : off+size]
		switch typ {
		case "ftyp":
			// brand box, no payload semantics for us
		case "tkhd":
			if len(body) < 36 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "tkhd too short")
			}
			for i := 0; i < 9; i++ {
				res.Matrix[i] = int32(binary.BigEndian.Uint32(body[i*4:]))
			}
			res.HasMatrix = true
		case "clap":
			if len(body) < 16 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "clap too short")
			}
			res.Crop = &Crop{
				X: int(int32(binary.BigEndian.Uint32(body[0:4]))),
				Y: int(int32(binary.BigEndian.Uint32(body[4:8]))),
				W: int(int32(binary.BigEndian.Uint32(body[8:12]))),
				H: int(int32(binary.BigEndian.Uint32(body[12:16]))),
			}
		case "colr":
			if len(body) < 3 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "colr too short")
			}
			c := ColorDesc{Primaries: body[0], Transfer: body[1], Matrix: body[2], Source: src + ":colr"}
			if res.Color != nil && *res.Color != c {
				return nil, media.Faultf(media.StageParse, track, int64(off),
					"conflicting colr boxes (%v vs %v): same-level color conflict",
					*res.Color, c)
			}
			res.Color = &c
		case "anch":
			if len(body) < 1 || len(body) < 1+int(body[0])*16 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "malformed anch box")
			}
			for i := 0; i < int(body[0]); i++ {
				p := body[1+i*16:]
				res.Anchors = append(res.Anchors, media.Anchor{
					Local: int64(binary.BigEndian.Uint64(p[0:8])),
					Ref:   int64(binary.BigEndian.Uint64(p[8:16])),
				})
			}
		case "apkt":
			if len(body) < 16 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "apkt too short")
			}
			res.Packets = append(res.Packets, &media.Packet{
				Track: track, Seq: int64(binary.BigEndian.Uint32(body[8:12])), Kind: media.KindAudio,
				PTS:      int64(binary.BigEndian.Uint64(body[0:8])),
				Duration: media.TicksToNS(int64(binary.BigEndian.Uint32(body[12:16])), media.AudioRate),
				Offset:   int64(off), Source: src, Data: bytes.Clone(body[16:]),
			})
			if len(res.Packets) > media.MaxPackets {
				return nil, media.Faultf(media.StageParse, track, int64(off), "packet count exceeds limit")
			}
		case "par1":
			pi, err := parseParity(body, track, int64(off))
			if err != nil {
				return nil, err
			}
			res.Parity = append(res.Parity, *pi)
		default:
			return nil, media.Faultf(media.StageParse, track, int64(off),
				"unsupported box type %q", typ)
		}
		off += size
	}
	return res, nil
}

func parseParity(body []byte, track int, off int64) (*ParityInfo, error) {
	if len(body) < 8 {
		return nil, media.Faultf(media.StageParse, track, off, "par1 too short")
	}
	pi := &ParityInfo{
		BaseSeq: int64(binary.BigEndian.Uint32(body[0:4])),
		Count:   int(binary.BigEndian.Uint32(body[4:8])),
	}
	need := 8 + pi.Count*16
	if pi.Count <= 0 || len(body) < need {
		return nil, media.Faultf(media.StageParse, track, off, "malformed par1 headers")
	}
	for i := 0; i < pi.Count; i++ {
		p := body[8+i*16:]
		pi.PTS = append(pi.PTS, int64(binary.BigEndian.Uint64(p[0:8])))
		pi.Samples = append(pi.Samples, int(binary.BigEndian.Uint32(p[8:12])))
		pi.Lengths = append(pi.Lengths, int(binary.BigEndian.Uint32(p[12:16])))
	}
	pi.XOR = bytes.Clone(body[need:])
	return pi, nil
}
