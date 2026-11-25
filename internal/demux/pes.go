// Package demux assembles PES packets into elementary tracks.
package demux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/pairwise/mediapack/internal/timeline"
	"github.com/pairwise/mediapack/internal/transport"
)

var (
	ErrNeedMore    = errors.New("PES needs more TS payload fragments")
	ErrInvalidPES  = errors.New("invalid PES packet")
	ErrUnsupported = errors.New("unsupported elementary codec")
)

// Frame is one assembled elementary media frame.
type Frame struct {
	PID        uint16
	Codec      string
	PTS        time.Duration
	PTSTicks   uint64
	KeyFrame   bool
	Sequence   uint32
	Payload    []byte
	PSIVersion int
}

// Track describes an active elementary stream.
type Track struct {
	PID        uint16
	Codec      string
	PSIVersion int
}

type pesBuilder struct {
	data []byte
}

// Demuxer assembles PES fragments by PID.
type Demuxer struct {
	tracks   map[uint16]Track
	builders map[uint16]*pesBuilder
}

func NewDemuxer() *Demuxer {
	return &Demuxer{tracks: map[uint16]Track{}, builders: map[uint16]*pesBuilder{}}
}

// SetTrack installs or replaces a track after a consistent PMT update.
func (d *Demuxer) SetTrack(pid uint16, codec string, psiVersion int) error {
	if !SupportedCodec(codec) {
		return timeline.NewError(timeline.ErrUnsupportedCodec,
			fmt.Sprintf("PID 0x%x codec %q is not supported", pid, codec), ErrUnsupported)
	}
	d.tracks[pid] = Track{PID: pid, Codec: codec, PSIVersion: psiVersion}
	return nil
}

// Tracks returns the currently configured tracks.
func (d *Demuxer) Tracks() []Track {
	out := make([]Track, 0, len(d.tracks))
	for _, t := range d.tracks {
		out = append(out, t)
	}
	return out
}

// SupportedCodec reports the deliberately small reusable kernel codec set.
// PCM audio is processed for energy; synthetic H.264-like keyframe metadata
// is used because fixtures are generated without external media.
func SupportedCodec(codec string) bool {
	switch codec {
	case "pcm_s16le", "h264-synthetic":
		return true
	default:
		return false
	}
}

// Push consumes one TS packet and returns at most one completed frame.
func (d *Demuxer) Push(p transport.Packet) (*Frame, error) {
	track, known := d.tracks[p.PID]
	if !known {
		// PSI and unknown PIDs are handled elsewhere; ignore safely.
		return nil, nil
	}

	b := d.builders[p.PID]
	if b == nil {
		b = &pesBuilder{}
		d.builders[p.PID] = b
	}
	if p.PUSI {
		// A PUSI starts a new PES. Generated PES lengths are exact, so any
		// retained bytes mean a malformed/truncated prior packet.
		b.data = b.data[:0]
	}
	b.data = append(b.data, p.Payload...)

	pesPayload, ptsTicks, err := ParsePES(b.data)
	if errors.Is(err, ErrNeedMore) {
		return nil, nil
	}
	if err != nil {
		b.data = b.data[:0]
		return nil, err
	}

	// Copy out and reset before parsing so later appends cannot alias Frame.
	pesPayload = append([]byte(nil), pesPayload...)
	b.data = b.data[:0]

	mh, media, err := ParseMediaFrame(pesPayload)
	if err != nil {
		return nil, err
	}
	return &Frame{
		PID:        p.PID,
		Codec:      track.Codec,
		PTSTicks:   ptsTicks,
		PTS:        timeline.PCRDuration(ptsTicks),
		KeyFrame:   mh.KeyFrame,
		Sequence:   mh.Sequence,
		Payload:    media,
		PSIVersion: track.PSIVersion,
	}, nil
}

// ParsePES parses one complete PES packet and returns its payload and PTS.
// It returns ErrNeedMore when the configured PES length has not arrived.
func ParsePES(data []byte) (payload []byte, ptsTicks uint64, err error) {
	if len(data) < 6 {
		return nil, 0, ErrNeedMore
	}
	if data[0] != 0 || data[1] != 0 || data[2] != 1 {
		return nil, 0, fmt.Errorf("%w: missing PES start code", ErrInvalidPES)
	}
	streamID := data[3]
	if (streamID < 0xBD || streamID > 0xEF) && streamID != 0xFA {
		return nil, 0, fmt.Errorf("%w: unsupported PES stream id 0x%02x", ErrInvalidPES, streamID)
	}
	packetLen := int(binary.BigEndian.Uint16(data[4:6]))
	var end int
	if packetLen == 0 {
		end = len(data) // unbounded video PES; generated streams avoid this
	} else {
		end = 6 + packetLen
		if len(data) < end {
			return nil, 0, ErrNeedMore
		}
	}
	if end < 9 || end > len(data) {
		return nil, 0, fmt.Errorf("%w: bad PES end %d len %d", ErrInvalidPES, end, len(data))
	}

	// Padding bytes after an exact PES may be present in the last TS packet.
	headerDataLen := int(data[8])
	payloadStart := 9 + headerDataLen
	if payloadStart > end {
		return nil, 0, fmt.Errorf("%w: PES header length overruns packet", ErrInvalidPES)
	}
	if data[7]&0x80 != 0 { // PTS_DTS_flags top bit: PTS present
		if headerDataLen < 5 || payloadStart < 14 {
			return nil, 0, fmt.Errorf("%w: PTS flagged but header too short", ErrInvalidPES)
		}
		b := data[9:14]
		if b[0]&0xf0 != 0x20 {
			return nil, 0, fmt.Errorf("%w: bad PTS prefix 0x%02x", ErrInvalidPES, b[0])
		}
		ptsTicks = uint64(b[0]&0x0e)<<29 |
			uint64(b[1])<<22 |
			uint64(b[2]&0xfe)<<14 |
			uint64(b[3])<<7 |
			uint64(b[4])>>1
	}
	return data[payloadStart:end], ptsTicks, nil
}

// MediaHeader is the deterministic in-PES frame marker used by fixtures.
type MediaHeader struct {
	KeyFrame bool
	Sequence uint32
}

const mediaMagic = "MPCK"
const mediaHeaderSize = 12

// BuildMediaFrame creates a small canonical elementary payload.
func BuildMediaFrame(seq uint32, keyFrame bool, body []byte) []byte {
	out := make([]byte, mediaHeaderSize+len(body))
	copy(out[0:4], mediaMagic)
	// Layout: [magic 4][flags 1][reserved 3][sequence uint32][body...].
	if keyFrame {
		out[4] = 0x01
	}
	binary.BigEndian.PutUint32(out[8:12], seq)
	copy(out[12:], body)
	return out
}

// ParseMediaFrame strips the deterministic marker and returns canonical body.
func ParseMediaFrame(data []byte) (MediaHeader, []byte, error) {
	if len(data) < mediaHeaderSize || string(data[0:4]) != mediaMagic {
		return MediaHeader{}, nil, fmt.Errorf("%w: missing media marker", ErrInvalidPES)
	}
	h := MediaHeader{KeyFrame: data[4]&0x01 != 0, Sequence: binary.BigEndian.Uint32(data[8:12])}
	return h, append([]byte(nil), data[mediaHeaderSize:]...), nil
}

// BuildPES builds a PTS-only PES packet.
func BuildPES(streamID byte, ptsTicks uint64, payload []byte) []byte {
	pts := encodePTS(ptsTicks)
	headerDataLen := byte(len(pts))
	// bytes after PES_packet_length: flags byte 1, flags byte 1, header len 1, header data.
	packetLen := 3 + int(headerDataLen) + len(payload)
	out := make([]byte, 6+packetLen)
	out[0], out[1], out[2], out[3] = 0, 0, 1, streamID
	binary.BigEndian.PutUint16(out[4:6], uint16(packetLen))
	out[6] = 0x80 // MPEG2 flags
	out[7] = 0x80 // PTS present
	out[8] = headerDataLen
	copy(out[9:14], pts)
	copy(out[9+len(pts):], payload)
	return out
}

func encodePTS(pts uint64) []byte {
	return []byte{
		0x21 | byte((pts>>29)&0x0e),
		byte(pts >> 22),
		0x01 | byte((pts>>14)&0xfe),
		byte(pts >> 7),
		0x01 | byte((pts<<1)&0xfe),
	}
}
