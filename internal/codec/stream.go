package codec

import (
	"encoding/binary"
	"io"

	"mediapack/internal/fault"
)

// StreamMeta summarizes a parsed elementary stream for the timeline and for
// muxer configuration-record generation.
type StreamMeta struct {
	Codec       Codec
	TrackID     uint32
	NumAU       int
	Keyframes   []int64 // AU sequence numbers that are keyframes
	Configs     [][]byte
	FirstPTS    int64
	LastPTS     int64
	BytesParsed int64
}

// frameTrailer is appended to every synthetic frame unit so truncation can be
// located to the exact byte where the declared length stops matching.
// Layout: "MPK1" + uint32 big-endian declared payload length.
var frameTrailerMagic = [4]byte{'M', 'P', 'K', '1'}

const frameTrailerLen = 8

type nalClassify func(raw []byte) (nalType uint8, key, config bool, err error)
type nalIsAUD func(nalType uint8) bool

// parseAnnexB implements the shared framing loop for all three codecs.
func parseAnnexB(r io.Reader, trackID uint32, c Codec,
	classify nalClassify, isAUD nalIsAUD) ([]AccessUnit, *StreamMeta, error) {

	sc := newAnnexBScanner(r, MaxNALBytes)
	var (
		aus       []AccessUnit
		meta      = &StreamMeta{Codec: c, TrackID: trackID, FirstPTS: -1}
		cur       *AccessUnit
		seq       int64
		pts       int64
		auStart   int64
		curKey    bool
	)

	flush := func() error {
		if cur == nil {
			return nil
		}
		cur.Key = curKey
		cur.ByteLen = int(meta.BytesParsed - auStart)
		if curKey {
			meta.Keyframes = append(meta.Keyframes, cur.Seq)
		}
		aus = append(aus, *cur)
		cur, curKey = nil, false
		return nil
	}

	for {
		raw, off, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		meta.BytesParsed = off + int64(len(raw))
		t, key, config, err := classify(raw)
		if err != nil {
			return nil, nil, fault.At(fault.KindCorruptInput, "parse", off, err.Error())
		}
		if config {
			meta.Configs = append(meta.Configs, append([]byte(nil), raw...))
		}
		if isAUD(t) {
			if err := flush(); err != nil {
				return nil, nil, err
			}
			auStart = off
			cur = &AccessUnit{Codec: c, TrackID: trackID, Seq: seq, PTS: pts,
				ByteOffset: off}
			seq++
			pts += frameDuration
			continue
		}
		if cur == nil {
			// Parameter sets preceding the first AUD are metadata, not frames.
			if config {
				continue
			}
			return nil, nil, fault.At(fault.KindCorruptInput, "parse", off,
				"frame unit appears before any access-unit delimiter")
		}
		if trailerRequired(c, t) {
			if err := verifyTrailer(raw, off, c); err != nil {
				return nil, nil, err
			}
		}
		cur.NALs = append(cur.NALs, NAL{Type: t, Offset: off, Raw: raw, Key: key, Config: config})
		curKey = curKey || key
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	if len(aus) == 0 {
		return nil, nil, fault.New(fault.KindCorruptInput, "parse", "no access units found")
	}
	meta.NumAU = len(aus)
	meta.FirstPTS, meta.LastPTS = aus[0].PTS, aus[len(aus)-1].PTS
	return aus, meta, nil
}

// trailerRequired reports whether a unit carries the synthetic integrity
// trailer. Access-unit delimiters are header-only and are exempt.
func trailerRequired(c Codec, t uint8) bool {
	switch c {
	case H264:
		return t != h264AUDType
	case H265:
		return t != h265AUDType
	default:
		return t != av1OBUTemporalDel
	}
}

// verifyTrailer checks the synthetic per-unit integrity trailer and reports
// the first byte where a truncated unit diverges from its declared length.
func verifyTrailer(raw []byte, off int64, c Codec) error {
	if len(raw) < frameTrailerLen {
		// Truncation landed inside the unit: the first missing byte is the
		// byte immediately following what the input still contains.
		return fault.At(fault.KindCorruptInput, "parse", off+int64(len(raw)),
			"truncated unit: integrity trailer missing")
	}
	tail := raw[len(raw)-frameTrailerLen:]
	if tail[0] != frameTrailerMagic[0] || tail[1] != frameTrailerMagic[1] ||
		tail[2] != frameTrailerMagic[2] || tail[3] != frameTrailerMagic[3] {
		return fault.At(fault.KindCorruptInput, "parse",
			off+int64(len(raw)-frameTrailerLen), "corrupt unit: bad integrity marker")
	}
	declared := binary.BigEndian.Uint32(tail[4:])
	if int(declared) != len(raw)-frameTrailerLen {
		// First bad byte is right after the bytes actually present.
		return fault.At(fault.KindCorruptInput, "parse", off+int64(len(raw)),
			"truncated NAL: declared payload length exceeds bytes available")
	}
	return nil
}
