// Package ingest reads elementary video/audio essence and JSON keyframe indexes.
package ingest

import (
	"encoding/binary"
	"fmt"

	"mediacheck/internal/media"
)

// Codec identifies a supported elementary essence.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecH265 Codec = "h265"
	CodecAV1  Codec = "av1"
	CodecPCM  Codec = "s16le"
)

// ParseCodec rejects unknown codecs instead of guessing.
func ParseCodec(s string) (Codec, error) {
	switch Codec(s) {
	case CodecH264, CodecH265, CodecAV1, CodecPCM:
		return Codec(s), nil
	default:
		return "", media.E(media.StageIngest, media.KindUnknownCodec, -1,
			fmt.Errorf("unsupported codec %q", s))
	}
}

// Unit is one framed NAL or OBU. Offset points at its start code/header.
type Unit struct {
	Codec        Codec
	Offset       int64
	HeaderLength int
	Payload      []byte
	Type         int
	Keyframe     bool
	PTS          int64
	Duration     int64
}

// ParseRequest contains raw essence and its mandatory clock declaration.
type ParseRequest struct {
	Codec         Codec
	Data          []byte
	Clock         *media.ClockDeclaration
	FrameDuration int64
	MaxBytes      int64
}

// ParseElementary parses H.264/H.265 Annex-B or length-framed AV1 OBUs.
func ParseElementary(req ParseRequest) ([]Unit, error) {
	if req.Clock == nil {
		return nil, media.E(media.StageIngest, media.KindMissingClock, -1, nil)
	}
	if err := req.Clock.Validate(media.StageIngest); err != nil {
		return nil, err
	}
	if req.MaxBytes > 0 && int64(len(req.Data)) > req.MaxBytes {
		return nil, media.E(media.StageIngest, media.KindResource, 0,
			fmt.Errorf("elementary stream is %d bytes, limit=%d", len(req.Data), req.MaxBytes))
	}
	switch req.Codec {
	case CodecH264, CodecH265:
		return parseAnnexB(req)
	case CodecAV1:
		return parseOBU(req)
	case CodecPCM:
		return nil, media.E(media.StageIngest, media.KindInvalidArgument, -1,
			fmt.Errorf("use ReadS16LE for PCM"))
	default:
		return nil, media.E(media.StageIngest, media.KindUnknownCodec, -1,
			fmt.Errorf("codec %q", req.Codec))
	}
}

func findStartCode(data []byte, from int) (start, headerLen int, next int, found bool) {
	for i := from; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		if data[i+2] == 1 {
			return i, 3, i + 3, true
		}
		if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			return i, 4, i + 4, true
		}
	}
	return 0, 0, 0, false
}

func parseAnnexB(req ParseRequest) ([]Unit, error) {
	data := req.Data
	if len(data) < 4 {
		return nil, media.E(media.StageIngest, media.KindTruncated, int64(len(data)),
			fmt.Errorf("annex-b stream is too short"))
	}
	var units []Unit
	_, headerLen, pos, found := findStartCode(data, 0)
	if !found {
		return nil, media.E(media.StageIngest, media.KindInvalidArgument, 0,
			fmt.Errorf("missing annex-b start code"))
	}
	framePTS := req.Clock.OriginPTS
	for found {
		start := pos - headerLen
		nextStart, nextHeader, nextPos, nextFound := findStartCode(data, pos)
		end := len(data)
		if nextFound {
			end = nextStart
		}
		payload := data[pos:end]
		if len(payload) < 2 {
			return nil, media.E(media.StageIngest, media.KindTruncated, int64(start),
				fmt.Errorf("NAL at offset %d has no header/payload", start))
		}
		nalType, key := nalTypeAndKey(req.Codec, payload)
		isVCL := isVCL(req.Codec, nalType)
		pts := framePTS
		duration := int64(0)
		if isVCL {
			if req.FrameDuration <= 0 {
				return nil, media.E(media.StageIngest, media.KindInvalidArgument, int64(start),
					fmt.Errorf("frame duration required for VCL NAL"))
			}
			duration = req.FrameDuration
			framePTS += req.FrameDuration
		}
		units = append(units, Unit{
			Codec: req.Codec, Offset: int64(start), HeaderLength: headerLen,
			Payload: payload, Type: nalType, Keyframe: key, PTS: pts, Duration: duration,
		})
		if !nextFound {
			break
		}
		_, headerLen, pos = nextStart, nextHeader, nextPos
		found = true
	}
	return units, nil
}

func nalTypeAndKey(codec Codec, nal []byte) (int, bool) {
	if len(nal) == 0 {
		return -1, false
	}
	if codec == CodecH264 {
		t := int(nal[0] & 0x1f)
		return t, t == 5
	}
	t := int((nal[0] >> 1) & 0x3f)
	return t, t == 19 || t == 20 || t == 21
}

func isVCL(codec Codec, nalType int) bool {
	if codec == CodecH264 {
		return nalType >= 1 && nalType <= 5
	}
	return nalType >= 0 && nalType < 32
}

func parseOBU(req ParseRequest) ([]Unit, error) {
	data := req.Data
	var units []Unit
	pos := 0
	framePTS := req.Clock.OriginPTS
	for pos < len(data) {
		start := pos
		if pos >= len(data) {
			break
		}
		header := data[pos]
		if header&0x80 != 0 {
			return nil, media.E(media.StageIngest, media.KindInvalidArgument, int64(pos),
				fmt.Errorf("AV1 forbidden bit set"))
		}
		obuType := int((header >> 3) & 0x0f)
		extension := header&0x04 != 0
		hasSize := header&0x02 != 0
		pos++
		if extension {
			if pos >= len(data) {
				return nil, media.E(media.StageIngest, media.KindTruncated, int64(start),
					fmt.Errorf("truncated AV1 extension"))
			}
			pos++
		}
		var payloadSize uint64
		if hasSize {
			shift := 0
			for {
				if pos >= len(data) {
					return nil, media.E(media.StageIngest, media.KindTruncated, int64(start),
						fmt.Errorf("truncated AV1 LEB128 size"))
				}
				b := data[pos]
				pos++
				payloadSize |= uint64(b&0x7f) << shift
				if b&0x80 == 0 {
					break
				}
				shift += 7
				if shift > 63 {
					return nil, media.E(media.StageIngest, media.KindInvalidArgument, int64(start),
						fmt.Errorf("AV1 LEB128 too long"))
				}
			}
		} else {
			payloadSize = uint64(len(data) - pos)
		}
		end := pos + int(payloadSize)
		if payloadSize > uint64(len(data)) || end > len(data) || end < pos {
			return nil, media.E(media.StageIngest, media.KindTruncated, int64(start),
				fmt.Errorf("OBU type=%d declared size=%d ends beyond %d bytes",
					obuType, payloadSize, len(data)))
		}
		isFrame := obuType == 6 || obuType == 3
		pts := framePTS
		var dur int64
		if isFrame {
			if req.FrameDuration <= 0 {
				return nil, media.E(media.StageIngest, media.KindInvalidArgument, int64(start),
					fmt.Errorf("frame duration required for AV1 frame OBU"))
			}
			dur = req.FrameDuration
			framePTS += req.FrameDuration
		}
		units = append(units, Unit{
			Codec: req.Codec, Offset: int64(start), HeaderLength: pos - start,
			Payload: data[pos:end], Type: obuType, PTS: pts, Duration: dur,
		})
		pos = end
	}
	return units, nil
}

// PCMFormat describes interleaved signed 16-bit little-endian audio.
type PCMFormat struct {
	SampleRate int
	Channels   int
}

// PCMStream is a validated s16le stream.
type PCMStream struct {
	Format     PCMFormat
	Data       []byte
	Clock      media.ClockDeclaration
	FrameCount int
}

// ReadS16LE validates PCM framing and derives its duration on the 90 kHz base.
func ReadS16LE(data []byte, format PCMFormat, clock *media.ClockDeclaration, maxBytes int64) (*PCMStream, error) {
	if clock == nil {
		return nil, media.E(media.StageIngest, media.KindMissingClock, -1, nil)
	}
	if err := clock.Validate(media.StageIngest); err != nil {
		return nil, err
	}
	if format.SampleRate <= 0 || format.Channels <= 0 {
		return nil, media.E(media.StageIngest, media.KindInvalidArgument, -1,
			fmt.Errorf("invalid PCM format %+v", format))
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, media.E(media.StageIngest, media.KindResource, 0,
			fmt.Errorf("PCM stream is %d bytes, limit=%d", len(data), maxBytes))
	}
	frameBytes := 2 * format.Channels
	if len(data)%frameBytes != 0 {
		offset := int64(len(data) - (len(data) % frameBytes))
		return nil, media.E(media.StageIngest, media.KindTruncated, offset,
			fmt.Errorf("s16le length %d is not a whole %d-byte frame", len(data), frameBytes))
	}
	return &PCMStream{
		Format: format, Data: data, Clock: *clock,
		FrameCount: len(data) / frameBytes,
	}, nil
}

// Int16Samples decodes interleaved s16le samples.
func (p *PCMStream) Int16Samples() []int16 {
	out := make([]int16, len(p.Data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(p.Data[2*i:]))
	}
	return out
}
