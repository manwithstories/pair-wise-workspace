// Package synth generates the deterministic MPEG-TS fixtures that every
// cinepipe test and the demo share. Everything is driven by an explicit seed so
// two runs produce identical bytes, which is what makes the cache hash and the
// manifest reproducible across runs.
package synth

import (
	"fmt"
	"math/rand"

	"cinepipe/internal/clock"
	"cinepipe/internal/ts"
)

// SeededRand carries the generator state as an embedded source. It is a type
// alias rather than a shared global so two fixtures built in one test cannot
// perturb one another.
type SeededRand struct{ *rand.Rand }

// NewRand returns a deterministic source for a seed.
func NewRand(seed int64) *SeededRand {
	return &SeededRand{rand.New(rand.NewSource(seed))}
}

// Options configures the muxer for a sample.
type Options struct {
	StreamID      uint16
	Program       uint16
	PMTPID        uint16
	VideoPID      uint16
	AudioPID      uint16
	PCRPID        uint16
	AudioType     uint8
	AudioLang     string
	AudioRate     int
	AudioChannels int
	// PSIVersion starts the PAT/PMT version byte; variants bump it mid-stream.
	PSIVersion uint8
}

// Muxer packetises elementary streams into a transport multiplex. PIDs and
// continuity counters are assigned as packets are written; the muxer owns the
// PAT/PMT version and can inject PSI updates mid-stream.
type Muxer struct {
	streamID   uint16
	program    uint16
	pmtPID     uint16
	pcrPID     uint16
	videoPID   uint16
	audioPID   uint16
	videoType  uint8
	audioType  uint8
	lang       string
	audioRate  int
	audioChans int

	psiVersion uint8
	cc         map[uint16]uint8
	out        []byte
}

// NewMuxer returns a muxer with the given layout and sane defaults.
func NewMuxer(opts Options) *Muxer {
	if opts.StreamID == 0 {
		opts.StreamID = 1
	}
	if opts.Program == 0 {
		opts.Program = 1
	}
	if opts.PMTPID == 0 {
		opts.PMTPID = 0x100
	}
	if opts.VideoPID == 0 {
		opts.VideoPID = 0x101
	}
	if opts.AudioPID == 0 {
		opts.AudioPID = 0x102
	}
	if opts.PCRPID == 0 {
		opts.PCRPID = opts.VideoPID
	}
	if opts.AudioType == 0 {
		opts.AudioType = ts.StreamTypeAAC
	}
	if opts.AudioLang == "" {
		opts.AudioLang = "eng"
	}
	if opts.AudioRate == 0 {
		opts.AudioRate = 48000
	}
	if opts.AudioChannels == 0 {
		opts.AudioChannels = 2
	}
	return &Muxer{
		streamID:   opts.StreamID,
		program:    opts.Program,
		pmtPID:     opts.PMTPID,
		pcrPID:     opts.PCRPID,
		videoPID:   opts.VideoPID,
		audioPID:   opts.AudioPID,
		videoType:  ts.StreamTypeH264,
		audioType:  opts.AudioType,
		lang:       opts.AudioLang,
		audioRate:  opts.AudioRate,
		audioChans: opts.AudioChannels,
		psiVersion: opts.PSIVersion,
		cc:         make(map[uint16]uint8),
	}
}

// Bytes returns the multiplex built so far.
func (m *Muxer) Bytes() []byte { return m.out }

// VideoPID, AudioPID, PMTPID, and Program expose the layout for tests.
func (m *Muxer) VideoPID() uint16 { return m.videoPID }
func (m *Muxer) AudioPID() uint16 { return m.audioPID }
func (m *Muxer) PMTPID() uint16   { return m.pmtPID }
func (m *Muxer) Program() uint16  { return m.program }
func (m *Muxer) PCRPID() uint16   { return m.pcrPID }

// BumpPSIVersion increments the PSI version, as a table update would.
func (m *Muxer) BumpPSIVersion() { m.psiVersion = (m.psiVersion + 1) & 0x1F }

// WritePSI emits one PAT and one PMT at the current version.
func (m *Muxer) WritePSI() {
	m.writePAT()
	m.writePMT()
}

func (m *Muxer) writePAT() {
	body := []byte{
		uint8(m.program >> 8), uint8(m.program),
		0xE0 | uint8(m.pmtPID>>8)&0x1F, uint8(m.pmtPID),
	}
	sec := ts.BuildSection(ts.TIDPat, m.streamID, m.psiVersion, body)
	m.writeSection(0, sec)
}

func (m *Muxer) writePMT() {
	var body []byte
	body = append(body, 0xE0|uint8(m.pcrPID>>8)&0x1F, uint8(m.pcrPID))
	body = append(body, 0xF0, 0x00) // no program info
	body = append(body, m.videoType, 0xE0|uint8(m.videoPID>>8)&0x1F, uint8(m.videoPID), 0xF0, 0x00)
	esInfo := []byte{0x0A, 0x04, m.lang[0], m.lang[1], m.lang[2], 0x00}
	body = append(body, m.audioType, 0xE0|uint8(m.audioPID>>8)&0x1F, uint8(m.audioPID),
		0xF0|uint8(len(esInfo)>>8)&0x0F, uint8(len(esInfo)))
	body = append(body, esInfo...)
	sec := ts.BuildSection(ts.TIDPmt, m.program, m.psiVersion, body)
	m.writeSection(m.pmtPID, sec)
}

// writeSection packetises a PSI section into TS packets with a pointer field.
func (m *Muxer) writeSection(pid uint16, sec []byte) {
	rest := append([]byte{0x00}, sec...) // pointer_field
	first := true
	for len(rest) > 0 {
		n := len(rest)
		if n > ts.PacketSize-4 {
			n = ts.PacketSize - 4
		}
		m.writePayloadPacket(pid, first, rest[:n])
		first = false
		rest = rest[n:]
	}
}

// writePayloadPacket emits a payload-bearing TS packet, stuffing the tail. The
// continuity counter advances by one on every payload packet, as the standard
// requires.
func (m *Muxer) writePayloadPacket(pid uint16, pusi bool, payload []byte) {
	p := make([]byte, ts.PacketSize)
	p[0] = ts.SyncByte
	if pusi {
		p[1] |= 0x40
	}
	p[1] |= uint8(pid>>8) & 0x1F
	p[2] = uint8(pid)
	cc := m.nextPayloadCC(pid)
	p[3] = 0x10 | cc&0x0F // payload only
	copy(p[4:], payload)
	for i := 4 + len(payload); i < len(p); i++ {
		p[i] = 0xFF
	}
	m.out = append(m.out, p...)
}

// nextPayloadCC returns the counter for the next payload packet on pid and
// records it as the last value used.
func (m *Muxer) nextPayloadCC(pid uint16) uint8 {
	last, ok := m.cc[pid]
	if !ok {
		m.cc[pid] = 0
		return 0
	}
	cc := (last + 1) & 0x0F
	m.cc[pid] = cc
	return cc
}

// currentCC returns the counter an adaptation-only packet carries: the value of
// the last packet on the PID, unchanged.
func (m *Muxer) currentCC(pid uint16) uint8 {
	if cc, ok := m.cc[pid]; ok {
		return cc
	}
	m.cc[pid] = 0
	return 0
}

// WritePCR emits an adaptation-only packet carrying a PCR on pid. An
// adaptation-only packet repeats its continuity counter, as the standard
// requires.
func (m *Muxer) WritePCR(pid uint16, base int64, discontinuity bool) {
	af := make([]byte, 0, ts.MaxAdaptationLen+1)
	flags := byte(0x10) // PCR flag
	if discontinuity {
		flags |= 0x80
	}
	af = append(af, 0, flags)
	af = append(af, ts.BuildPCRBytes(ts.PCR{Base: uint64(base)})...)
	for len(af) <= ts.MaxAdaptationLen {
		af = append(af, 0xFF)
	}
	af = af[:ts.MaxAdaptationLen+1]
	af[0] = uint8(len(af) - 1)

	p := make([]byte, ts.PacketSize)
	p[0] = ts.SyncByte
	p[1] |= uint8(pid>>8) & 0x1F
	p[2] = uint8(pid)
	p[3] = 0x20 | m.currentCC(pid)&0x0F // adaptation only; counter unchanged
	copy(p[4:], af)
	m.out = append(m.out, p...)
}

// WritePES packetises a PES payload onto pid, prepending a PES header with an
// optional PTS, splitting it across TS packets. It returns the byte offset of
// the first packet.
func (m *Muxer) WritePES(pid uint16, streamID uint8, payload []byte, pts int64, withPTS bool) int64 {
	hdr := pesHeader(streamID, pts, withPTS)
	data := append(hdr, payload...)
	first := int64(len(m.out))
	firstPacket := true
	for len(data) > 0 {
		n := len(data)
		if n > ts.PacketSize-4 {
			n = ts.PacketSize - 4
		}
		m.writePayloadPacket(pid, firstPacket, data[:n])
		firstPacket = false
		data = data[n:]
	}
	return first
}

// pesHeader builds a minimal PES header with an optional PTS.
func pesHeader(streamID byte, pts int64, withPTS bool) []byte {
	h := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x00, 0x00}
	if !withPTS {
		return h
	}
	h[7] = 0x80
	h[8] = 0x05
	return append(h, ptsBytes(uint64(pts))...)
}

// ptsBytes renders a 33-bit timestamp into the marker-bit layout: b[0] holds
// bits 32..30, b[2] bits 21..15, b[4] bits 6..0.
func ptsBytes(v uint64) []byte {
	return []byte{
		0x21 | byte((v>>30)&0x07)<<1 | 0x01,
		byte(v >> 22),
		byte((v>>15)&0x7F)<<1 | 0x01,
		byte(v >> 7),
		byte(v&0x7F)<<1 | 0x01,
	}
}

// WriteNull emits a null-PID stuffing packet.
func (m *Muxer) WriteNull() {
	p := make([]byte, ts.PacketSize)
	p[0] = ts.SyncByte
	p[1] |= 0x1F
	p[2] = 0xFF
	p[3] = 0x10
	for i := 4; i < len(p); i++ {
		p[i] = 0xFF
	}
	m.out = append(m.out, p...)
}

// DropLastPackets removes up to n trailing transport packets on the given PID
// from the output already written. The PID's continuity counter keeps its
// advanced value, so the next packet on that PID shows a jump — exactly the
// transport loss a damaged capture would exhibit. It returns the number
// removed.
func (m *Muxer) DropLastPackets(pid uint16, n int) int {
	var positions []int
	for off := 0; off+ts.PacketSize <= len(m.out); off += ts.PacketSize {
		p := m.out[off:]
		if uint16(p[1]&0x1F)<<8|uint16(p[2]) == pid {
			positions = append(positions, off)
		}
	}
	if len(positions) < n {
		n = len(positions)
	}
	cut := positions[len(positions)-n]
	end := cut + n*ts.PacketSize
	trimmed := make([]byte, 0, len(m.out)-(end-cut))
	trimmed = append(trimmed, m.out[:cut]...)
	trimmed = append(trimmed, m.out[end:]...)
	m.out = trimmed
	return n
}

// Format describes the muxer layout for diagnostics.
func (m *Muxer) Format() string {
	return fmt.Sprintf("program=%d pmt=0x%04x video=0x%04x audio=0x%04x pcr=0x%04x",
		m.program, m.pmtPID, m.videoPID, m.audioPID, m.pcrPID)
}

// TicksPerSecond is the muxer's notion of one second in 90 kHz ticks.
const TicksPerSecond = clock.Base
