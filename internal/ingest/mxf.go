package ingest

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"

	"archive/internal/media"
)

// Simplified MXF KLV keys (16-byte ULs). Only the CDCI descriptor,
// anchor and JPEG frame keys are accepted; anything else — including
// other essence descriptors such as MPEG2 video — is rejected.
var (
	keyCDCI   = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x01, 0x00}
	keyAnchor = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x02, 0x00}
	keyFrame  = [16]byte{0x06, 0x0E, 0x2B, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0D, 0x01, 0x02, 0x01, 0x01, 0x05, 0x03, 0x00}
)

// CDCIDescriptor is the MXF CDCI video descriptor subset we honor.
type CDCIDescriptor struct {
	Width, Height              int
	Primaries, Transfer, Matrix byte
	Rotation                   int // degrees: 0/90/180/270
}

// MXFResult is the parsed content of one MXF source.
type MXFResult struct {
	Descriptor CDCIDescriptor
	HasDesc    bool
	Anchors    []media.Anchor
	Packets    []*media.Packet // video frames, PTS still raw 33-bit
	Faults     []*media.Fault  // non-fatal problems (e.g. truncated tail)
}

// ParseMXF parses the supported KLV subset. Fatal problems (unsupported
// keys, malformed headers) return an error; a truncated trailing KLV is
// recorded in Faults with its byte offset and parsing stops.
func ParseMXF(path string, track int) (*MXFResult, error) {
	data, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	res := &MXFResult{}
	src := filepath.Base(path)
	off := 0
	for off < len(data) {
		if len(data)-off < 21 { // 16 key + 1 BER + 4 len
			res.Faults = append(res.Faults, media.Faultf(media.StageParse, track,
				int64(off), "truncated KLV header: %d bytes remain", len(data)-off))
			break
		}
		var key [16]byte
		copy(key[:], data[off:off+16])
		if data[off+16] != 0x84 {
			return nil, media.Faultf(media.StageParse, track, int64(off),
				"unsupported BER length form 0x%02x", data[off+16])
		}
		vlen := int(binary.BigEndian.Uint32(data[off+17 : off+21]))
		voff := off + 21
		if vlen < 0 || voff+vlen > len(data) {
			res.Faults = append(res.Faults, media.Faultf(media.StageParse, track,
				int64(off), "truncated KLV value: need %d bytes, %d remain", vlen, len(data)-voff))
			break
		}
		val := data[voff : voff+vlen]
		switch key {
		case keyCDCI:
			if vlen < 8 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "CDCI descriptor too short: %d", vlen)
			}
			res.Descriptor = CDCIDescriptor{
				Width:     int(binary.BigEndian.Uint16(val[0:2])),
				Height:    int(binary.BigEndian.Uint16(val[2:4])),
				Primaries: val[4], Transfer: val[5], Matrix: val[6],
				Rotation: int(val[7]),
			}
			res.HasDesc = true
		case keyAnchor:
			if vlen < 1 || vlen < 1+int(val[0])*16 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "malformed anchor KLV")
			}
			for i := 0; i < int(val[0]); i++ {
				p := val[1+i*16:]
				res.Anchors = append(res.Anchors, media.Anchor{
					Local: int64(binary.BigEndian.Uint64(p[0:8])),
					Ref:   int64(binary.BigEndian.Uint64(p[8:16])),
				})
			}
		case keyFrame:
			if vlen < 9 {
				return nil, media.Faultf(media.StageParse, track, int64(off), "frame KLV too short: %d", vlen)
			}
			pts := int64(binary.BigEndian.Uint32(val[0:4]))<<8 | int64(val[4])
			payload := bytes.Clone(val[9:])
			if !isJPEG(payload) {
				return nil, media.Faultf(media.StageParse, track, int64(off),
					"unsupported video essence (only JPEG accepted)")
			}
			res.Packets = append(res.Packets, &media.Packet{
				Track: track, Seq: int64(len(res.Packets)), Kind: media.KindVideo,
				PTS: pts, Duration: int64(binary.BigEndian.Uint32(val[5:9])),
				Offset: int64(off), Source: src, Data: payload,
			})
			if len(res.Packets) > media.MaxPackets {
				return nil, media.Faultf(media.StageParse, track, int64(off), "packet count exceeds limit")
			}
		default:
			return nil, media.Faultf(media.StageParse, track, int64(off),
				"unsupported KLV key %x… (only CDCI/JPEG subset accepted)", key[:6])
		}
		off = voff + vlen
	}
	return res, nil
}

func readCapped(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > media.MaxFileSize {
		return nil, media.Faultf(media.StageParse, -1, 0, "file %s too large: %d bytes", path, st.Size())
	}
	return os.ReadFile(path)
}

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}
