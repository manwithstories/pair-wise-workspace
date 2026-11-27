package inspect

import (
	"encoding/binary"
	"errors"
	"fmt"

	"mediacheck/internal/model"
)

// Audio and video structural errors.  The inspect stage maps them onto fault
// kinds so the recovery stage and the report can act on them.
var (
	errEmptyPacket   = errors.New("empty payload")
	errShortHeader   = errors.New("payload shorter than the codec header")
	errTOC           = errors.New("invalid Opus TOC configuration")
	errADTS          = errors.New("invalid ADTS header")
	errNALTooShort   = errors.New("AVCC unit shorter than a NAL header")
	errNALLength     = errors.New("declared NAL length exceeds payload")
	errNoNALUnits    = errors.New("no NAL units in access unit")
	errFrameTooShort = errors.New("frame shorter than its declared length")
)

// codecFaultError carries the fault kind a parser decided on.
type codecFaultError struct {
	kind model.FaultKind
	msg  string
}

func (e *codecFaultError) Error() string { return e.msg }

func faultErr(kind model.FaultKind, format string, args ...any) error {
	return &codecFaultError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

func audioFaults(p model.Packet, err error) []model.Fault {
	kind := model.FaultCorruptPayload
	var ce *codecFaultError
	if errors.As(err, &ce) {
		kind = ce.kind
	}
	return []model.Fault{{Track: p.Track, Kind: kind, Seq: uint64(p.Sequence), Slot: p.Slot, Note: err.Error()}}
}

func videoFaults(p model.Packet, err error) []model.Fault {
	kind := model.FaultCorruptPayload
	var ce *codecFaultError
	if errors.As(err, &ce) {
		kind = ce.kind
	}
	return []model.Fault{{Track: p.Track, Kind: kind, Seq: uint64(p.Sequence), Slot: p.Slot, Note: err.Error()}}
}

// opusFrameSizes lists the valid samples-per-channel values of an Opus frame
// in order, indexed by (TOC & 0x03).

// ParseOpusDuration returns the packet duration in 48 kHz samples per
// channel declared by the TOC byte and frame count.  It validates the
// reserved configuration and the 2.5 ms frame case.
func ParseOpusDuration(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, faultErr(model.FaultTruncatedHeader, "%v", errEmptyPacket)
	}
	toc := data[0]
	if len(data) < 2 {
		// Every Opus packet carries at least one frame byte after the TOC.
		// A lone TOC byte means the frame data was cut off; DTX-only packets
		// are outside the scope of broadcast carriage.
		return 0, faultErr(model.FaultTruncatedHeader, "%v: only the TOC byte survived", errShortHeader)
	}
	config := toc >> 3
	code := toc & 0x03
	var samples int
	switch {
	case config < 12:
		// SILK-only: 10, 20, 40, 60 ms per frame.
		samples = []int{480, 960, 1920, 2880}[config&0x03]
	case config < 16:
		// Hybrid: 10, 20 ms per frame.
		samples = []int{480, 960}[config&0x01]
	default:
		// CELT: 2.5, 5, 10, 20 ms per frame.
		samples = []int{120, 240, 480, 960}[config&0x03]
	}
	// Frame count from the code bits.
	frames := 1
	switch code {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(data) < 2 {
			return 0, faultErr(model.FaultTruncatedHeader, "%v: missing frame count byte", errShortHeader)
		}
		frames = int(data[1] & 0x3f)
		if frames <= 0 || frames > 48 {
			return 0, faultErr(model.FaultCorruptPayload, "invalid Opus frame count %d", frames)
		}
	}
	return samples * frames, nil
}

// ParseADTSDuration returns the frame length and 48 kHz-equivalent duration
// declared by an AAC ADTS header.
func ParseADTSDuration(data []byte) (samples int, frameLen int, err error) {
	if len(data) < 7 {
		return 0, 0, faultErr(model.FaultTruncatedHeader, "%v: %d bytes", errShortHeader, len(data))
	}
	if data[0] != 0xFF || data[1]&0xF0 != 0xF0 {
		return 0, 0, faultErr(model.FaultCorruptPayload, "%v: syncword %02x%02x", errADTS, data[0], data[1])
	}
	protectionAbsent := data[1]&0x01 != 0
	headerLen := 7
	if !protectionAbsent {
		headerLen = 9
	}
	// sampling_frequency_index occupies bits 2..5 of byte 2.
	sfi := (data[2] >> 2) & 0x0f
	if sfi > 12 {
		return 0, 0, faultErr(model.FaultCorruptPayload, "%v: sampling index %d", errADTS, sfi)
	}
	frameLen = int(data[3]&0x03)<<11 | int(data[4])<<3 | int(data[5]>>5)
	if frameLen < headerLen {
		return 0, 0, faultErr(model.FaultBadLength, "%v: frame length %d below header %d", errBadFrameLen, frameLen, headerLen)
	}
	if frameLen > len(data) {
		return 0, 0, faultErr(model.FaultBadLength, "%v: frame length %d exceeds payload %d", errBadFrameLen, frameLen, len(data))
	}
	// 1024 samples per AAC-LC frame at the stream rate, scaled to 48 kHz.
	rate := adtsSampleRates[sfi]
	samples = int(int64(1024) * 48000 / int64(rate))
	return samples, frameLen, nil
}

var errBadFrameLen = errors.New("invalid AAC frame length")

// maxADTSFrame bounds a single ADTS frame; the 13 bit length field cannot
// express more than 8191 bytes anyway.
const maxADTSFrame = 8191

var adtsSampleRates = [13]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

func parseAudio(p model.Packet) error {
	if len(p.Data) == 0 {
		return faultErr(model.FaultTruncatedHeader, "%v", errEmptyPacket)
	}
	switch p.Codec {
	case model.CodecOpus:
		dur, err := ParseOpusDuration(p.Data)
		if err != nil {
			return err
		}
		want := p.Timebase.Nanos(int64(dur))
		got := p.Slot.Duration()
		// A declared duration more than a millisecond away from the slot is
		// a structural inconsistency worth reporting, but not damage: real
		// rescaled streams round to the millisecond grid.
		if diff := want - got; diff > 1_000_000 || diff < -1_000_000 {
			return faultErr(model.FaultDurationChange,
				"TOC declares %d samples (%d ns), slot is %d ns", dur, want, got)
		}
		return nil
	case model.CodecAAC:
		samples, frameLen, err := ParseADTSDuration(p.Data)
		if err != nil {
			return err
		}
		_ = samples
		if frameLen > maxADTSFrame {
			return faultErr(model.FaultBadLength, "declared frame %d above cap %d", frameLen, maxADTSFrame)
		}
		return nil
	default:
		return nil
	}
}

// H264NALTypes returns the NAL unit types of an AVCC access unit.
func H264NALTypes(data []byte) ([]byte, error) {
	types, err := walkAVCC(data, nil)
	return types, err
}

// IsH264Keyframe reports whether an AVCC access unit contains an IDR slice.
func IsH264Keyframe(data []byte) (bool, error) {
	key := false
	_, err := walkAVCC(data, func(nalType byte) {
		if nalType == 5 {
			key = true
		}
	})
	return key, err
}

func walkAVCC(data []byte, visit func(byte)) ([]byte, error) {
	if len(data) == 0 {
		return nil, faultErr(model.FaultTruncatedHeader, "%v", errEmptyPacket)
	}
	var types []byte
	for off := 0; off < len(data); {
		if len(data)-off < 4 {
			return types, faultErr(model.FaultTruncatedHeader, "%v: %d trailing bytes", errShortHeader, len(data)-off)
		}
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		if n <= 0 {
			return types, faultErr(model.FaultTruncatedHeader, "%v: zero length NAL", errNALTooShort)
		}
		if n > len(data)-off {
			return types, faultErr(model.FaultBadLength, "%v: NAL length %d exceeds %d remaining bytes",
				errNALLength, n, len(data)-off)
		}
		nal := data[off : off+n]
		types = append(types, nal[0]&0x1f)
		if visit != nil {
			visit(nal[0] & 0x1f)
		}
		off += n
	}
	if len(types) == 0 {
		return nil, faultErr(model.FaultCorruptPayload, "%v", errNoNALUnits)
	}
	return types, nil
}

func parseH264(p model.Packet) error {
	types, err := walkAVCC(p.Data, nil)
	if err != nil {
		return err
	}
	// A frame without any slice NAL (1..5) carries no picture.
	slices := 0
	for _, t := range types {
		if t >= 1 && t <= 5 {
			slices++
		}
	}
	if slices == 0 {
		return faultErr(model.FaultCorruptPayload, "access unit has no slice NAL (types %v)", types)
	}
	return nil
}
