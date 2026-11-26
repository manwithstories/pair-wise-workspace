// Package sample synthesizes deterministic, self-contained RTP/RTCP fixtures
// carrying RFC 2250 MPEG-TS. No external media is read: every TS packet, NAL
// unit, sequence number and sender report is derived from a fixed seed so the
// output byte stream and every segment hash are reproducible across runs.
package sample

import "encoding/binary"

// MPEG-TS fixed parameters.
const (
	TSPacketSize = 188
	SyncByte     = 0x47

	PIDPAT = 0x0000
	PIDPMT = 0x1000
	PIDVideo = 0x0100

	// StreamPSIInterval inserts a PAT/PMT pair roughly this often (in TS
	// packets) so a resynchronized decoder can recover table context.
	StreamPSIInterval = 400
)

// Video timing: 25 fps on the 90 kHz RTP clock, an IDR every 2 seconds and a
// six-second segmentation target, giving exact keyframe-aligned cut points.
const (
	FramesPerSecond = 25
	ClockRate       = 90_000
	TicksPerFrame   = ClockRate / FramesPerSecond // 3600
	IDRPeriodFrame  = FramesPerSecond * 2         // IDR every 50 frames
	SegmentSeconds  = 6
	SegmentFrames   = FramesPerSecond * SegmentSeconds // 150 frames
)

// NAL unit types we synthesize (ITU-T H.264 section 7.4.1).
const (
	NALNonIDR = 1
	NALIDR    = 5
)

// Rendition describes one bitrate ladder rung.
type Rendition struct {
	Name      string // stable identifier, also used in output paths
	SSRC      uint32
	Bandwidth uint32 // bits per second, advertised in the HLS master
	Width     int
	Height    int
	// TSPerFrame is the fixed number of video TS packets per coded frame,
	// chosen so the rendition approximates its target bandwidth.
	TSPerFrame int
}

// DefaultRenditions returns the two-rung ladder used by every demo run. Both
// rungs share frame timing, so their segment boundaries line up exactly.
func DefaultRenditions() []Rendition {
	return []Rendition{
		{Name: "v1", SSRC: 0xC0FFEE01, Bandwidth: 1_200_000, Width: 854, Height: 480, TSPerFrame: 20},
		{Name: "v2", SSRC: 0xC0FFEE02, Bandwidth: 2_400_000, Width: 1280, Height: 720, TSPerFrame: 40},
	}
}

// ProgramDuration returns the number of frames for a program of n seconds.
func ProgramDuration(seconds int) int { return seconds * FramesPerSecond }

// writeHeader fills the 4-byte MPEG-TS header at b[0:4].
func writeHeader(b []byte, pid uint16, pusi, payloadStart bool, cc uint8) {
	b[0] = SyncByte
	if pusi {
		b[1] = 0x40
	}
	b[1] |= byte(pid >> 8)
	b[2] = byte(pid)
	// payload-only (no adaptation field) unless caller overwrites.
	b[3] = 0x10 | (cc & 0x0F)
	if !payloadStart {
		// keep payload unit indicator handled by caller; this flag is unused.
	}
}

// stuffing fills the remainder of a TS packet with 0xFF after length.
func stuffing(b []byte, from int) {
	for i := from; i < TSPacketSize; i++ {
		b[i] = 0xFF
	}
}

// patPacket builds a PAT pointing at the single PMT PID.
func patPacket(cc uint8) []byte {
	b := make([]byte, TSPacketSize)
	writeHeader(b, PIDPAT, true, true, cc)
	// pointer_field + minimal table section.
	b[4] = 0x00
	b[5] = 0x00 // table_id PAT
	length := 13
	binary.BigEndian.PutUint16(b[6:8], uint16(0xB000|(length&0x0FFF)))
	b[8] = 0x01 // transport_stream_id high
	b[9] = 0x01
	b[10] = 0x01 // version/version/current-next
	b[11] = 0x00 // section_number
	b[12] = 0x00 // last_section_number
	binary.BigEndian.PutUint16(b[13:15], 0x0001) // program number 1
	binary.BigEndian.PutUint16(b[15:17], 0xE000|PIDPMT)
	b[17] = 0x00 // CRC placeholder region; fixtures do not require a valid CRC
	stuffing(b, 18)
	return b
}

// pmtPacket builds a PMT naming the single video elementary stream.
func pmtPacket(cc uint8) []byte {
	b := make([]byte, TSPacketSize)
	writeHeader(b, PIDPMT, true, true, cc)
	b[4] = 0x00
	b[5] = 0x02 // table_id PMT
	length := 18
	binary.BigEndian.PutUint16(b[6:8], uint16(0xB000|(length&0x0FFF)))
	b[8], b[9] = 0x01, 0x01
	b[10] = 0x01
	b[11], b[12] = 0x00, 0x00
	binary.BigEndian.PutUint16(b[13:15], 0xE000|PIDVideo) // PCR PID
	b[15], b[16] = 0xF0, 0x00                             // no program info
	b[17] = 0x1B                                          // stream type H.264
	binary.BigEndian.PutUint16(b[18:20], 0xE000|PIDVideo)
	b[20], b[21] = 0xF0, 0x00 // no ES info
	stuffing(b, 22)
	return b
}
