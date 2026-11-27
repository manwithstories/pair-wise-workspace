// Package fixture generates deterministic MPEG-TS sample streams with
// optional injected faults. Sample data is produced from a seed only;
// no external files are referenced.
package fixture

import (
	"math/rand/v2"

	"tscheck/internal/ts"
)

// PIDs and stream types used by generated streams.
const (
	PATPID         = 0x0000
	PMTPID         = 0x1000
	VideoPID       = 0x0100
	StreamTypeH264 = 0x1B
)

// frameTicks is the nominal 30 fps frame duration on the 27 MHz base.
const frameTicks = 900_000

// esFillByBranch selects the per-frame ES payload size per bitrate branch.
var esFillByBranch = []int{180, 500, 1200}

// Options controls deterministic stream generation.
type Options struct {
	Seed   int64
	Bytes  int // approximate target size in bytes
	Branch int // 0..2: low/medium/high bitrate
	VFR    bool
	Faults FaultSpec
}

// FaultSpec selects injected faults.
type FaultSpec struct {
	PCRWrap   bool // start PCR just before the 33-bit wrap
	CCJump    bool // one continuity counter jump mid-stream
	PSIBump   bool // one legal PSI version update
	BadPSICRC bool // one PMT update with corrupted CRC (must be rejected)
	Truncate  bool // cut the tail mid-packet
}

// FaultOffsets records the byte offsets where faults were injected.
type FaultOffsets struct {
	CCJump     int64
	BadPSICRC  int64
	Truncation int64
}

// First returns the earliest injected fault offset, or -1 when the
// stream is clean.
func (f FaultOffsets) First() int64 {
	first := int64(-1)
	for _, off := range []int64{f.CCJump, f.BadPSICRC, f.Truncation} {
		if off >= 0 && (first < 0 || off < first) {
			first = off
		}
	}
	return first
}

// Generate builds a deterministic TS stream. The same options always
// produce byte-identical output.
func Generate(opts Options) ([]byte, FaultOffsets) {
	g := &generator{
		out:      make([]byte, 0, opts.Bytes+ts.PacketSize),
		cc:       map[uint16]uint8{},
		schedule: rand.New(rand.NewPCG(uint64(opts.Seed), uint64(opts.Seed)^0x9E3779B9)),
		fill:     rand.New(rand.NewPCG(uint64(opts.Seed)*10+uint64(opts.Branch), 0xA5A5)),
		opts:     opts,
		psiVer:   0,
	}
	g.run()
	return g.out, g.faults
}

type generator struct {
	out      []byte
	cc       map[uint16]uint8
	schedule *rand.Rand
	fill     *rand.Rand
	opts     Options
	faults   FaultOffsets
	psiVer   int
	frame    int
	ccJumpAt int
}

func (g *generator) run() {
	g.faults = FaultOffsets{CCJump: -1, BadPSICRC: -1, Truncation: -1}
	start := int64(27_000_000) // 1s on the 27 MHz base
	if g.opts.Faults.PCRWrap {
		start = (int64(1<<33) - 45000) * 300 // 0.5s before PCR base wrap
	}
	g.writePSI()
	t := start
	for len(g.out) < g.opts.Bytes {
		interval := int64(frameTicks)
		if g.opts.VFR {
			interval = frameTicks/2 + g.schedule.Int64N(frameTicks)
		}
		if g.opts.Faults.PSIBump && g.frame == 60 {
			g.psiVer = 1
			g.writePSI()
		}
		if g.opts.Faults.BadPSICRC && g.frame == 90 {
			g.faults.BadPSICRC = int64(len(g.out))
			g.writePMT(g.psiVer+1, true)
		}
		keyframe := g.frame%30 == 0
		g.writeFrame(t, keyframe)
		t += interval
		g.frame++
		if g.frame%30 == 0 {
			g.writePSI()
		}
	}
	if g.opts.Faults.Truncate && len(g.out) > 2*ts.PacketSize {
		g.faults.Truncation = int64(len(g.out) - 100)
		g.out = g.out[:len(g.out)-100]
	}
}

// writeFrame emits one video PES (PCR on its first packet).
func (g *generator) writeFrame(t int64, keyframe bool) {
	es := g.es(keyframe)
	pes := pesHeader(t/300, len(es))
	pes = append(pes, es...)
	cap := 176 // first packet carries a PCR
	first := true
	for len(pes) > 0 {
		n := min(cap, len(pes))
		pcr := int64(-1)
		if first {
			pcr = t
		}
		g.maybeCCJump()
		g.packet(VideoPID, first, pcr, pes[:n])
		pes = pes[n:]
		first = false
		cap = 184
	}
}

// maybeCCJump injects the continuity counter jump at frame 45.
func (g *generator) maybeCCJump() {
	if g.opts.Faults.CCJump && g.frame == 45 && g.faults.CCJump < 0 {
		g.faults.CCJump = int64(len(g.out))
		g.cc[VideoPID] += 2
	}
}

// es builds the H.264 Annex-B elementary stream chunk for one frame.
func (g *generator) es(keyframe bool) []byte {
	size := esFillByBranch[g.opts.Branch%len(esFillByBranch)]
	var es []byte
	if keyframe {
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1E) // SPS
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x68, 0xCE, 0x06, 0xE2) // PPS
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x65)                   // IDR
		size += size / 2
	} else {
		es = append(es, 0x00, 0x00, 0x00, 0x01, 0x41) // non-IDR slice
	}
	for len(es) < size {
		es = append(es, byte(g.fill.Uint32()))
	}
	return es
}

// packet appends one 188-byte TS packet. pcrTicks >= 0 attaches a PCR.
// The payload must fit the capacity left after the adaptation field.
func (g *generator) packet(pid uint16, pusi bool, pcrTicks int64, payload []byte) {
	pkt := make([]byte, ts.PacketSize)
	pkt[0] = ts.SyncByte
	if pusi {
		pkt[1] = 0x40
	}
	pkt[1] |= byte(pid >> 8)
	pkt[2] = byte(pid)
	hasPCR := pcrTicks >= 0
	body := ts.PacketSize - 4 // 184 bytes after the header
	pos := 4
	switch {
	case !hasPCR && len(payload) == body:
		pkt[3] = 0x10 // payload only
	case !hasPCR:
		pkt[3] = 0x30 // adaptation (stuffing) + payload
		afl := body - len(payload) - 1
		pkt[4] = byte(afl)
		pos = 5
		if afl > 0 {
			pkt[5] = 0x00
			pos = 6
		}
	default:
		pkt[3] = 0x30
		afl := body - len(payload) - 1
		pkt[4] = byte(afl)
		pkt[5] = 0x10 // PCR flag
		base := (pcrTicks / 300) % (1 << 33)
		ext := pcrTicks % 300
		pkt[6] = byte(base >> 25)
		pkt[7] = byte(base >> 17)
		pkt[8] = byte(base >> 9)
		pkt[9] = byte(base >> 1)
		pkt[10] = byte(base<<7) | byte(ext>>8)&0x01
		pkt[11] = byte(ext)
		pos = 12
	}
	pkt[3] |= g.cc[pid] & 0x0F
	g.cc[pid]++
	copy(pkt[pos:], payload)
	for i := pos + len(payload); i < ts.PacketSize; i++ {
		pkt[i] = 0xFF
	}
	g.out = append(g.out, pkt...)
}

// writePSI emits the current PAT and PMT versions.
func (g *generator) writePSI() {
	g.psiPacket(PATPID, patSection(g.psiVer))
	g.writePMT(g.psiVer, false)
}

// writePMT emits a PMT, optionally with a corrupted CRC.
func (g *generator) writePMT(version int, badCRC bool) {
	sec := pmtSection(version)
	if badCRC {
		sec[len(sec)-1] ^= 0xFF
	}
	g.psiPacket(PMTPID, sec)
}

// psiPacket wraps a section in one PSI packet with pointer_field 0.
func (g *generator) psiPacket(pid uint16, sec []byte) {
	payload := append([]byte{0x00}, sec...)
	g.packet(pid, true, -1, payload)
}

// patSection builds a PAT mapping program 1 to PMTPID.
func patSection(version int) []byte {
	body := []byte{
		0x00, 0xB0, 0x0D,
		0x00, 0x01,
		0xC1 | byte(version<<1),
		0x00, 0x00,
		0x00, 0x01,
		0xE0 | byte(PMTPID>>8), byte(PMTPID&0xFF),
	}
	return appendCRC(body)
}

// pmtSection builds a PMT with one H.264 video stream.
func pmtSection(version int) []byte {
	body := []byte{
		0x02, 0xB0, 0x12,
		0x00, 0x01,
		0xC1 | byte(version<<1),
		0x00, 0x00,
		0xE0 | byte(VideoPID>>8), byte(VideoPID&0xFF),
		0xF0, 0x00,
		StreamTypeH264,
		0xE0 | byte(VideoPID>>8), byte(VideoPID&0xFF),
		0xF0, 0x00,
	}
	return appendCRC(body)
}

// appendCRC appends the CRC-32/MPEG-2 of body to body.
func appendCRC(body []byte) []byte {
	crc := ts.CRC32MPEG(body)
	return append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// pesHeader builds a PES header (start prefix through PTS) for esLen
// bytes of ES data with the given raw 33-bit 90 kHz PTS.
func pesHeader(ptsRaw int64, esLen int) []byte {
	pts := ptsRaw % (1 << 33)
	declared := 3 + 5 + esLen
	return []byte{
		0x00, 0x00, 0x01, 0xE0,
		byte(declared >> 8), byte(declared),
		0x80, 0x80, 0x05,
		byte(0x02<<4 | ((pts >> 30) & 0x07) << 1 | 1),
		byte(pts >> 22),
		byte(((pts >> 15) & 0x7F) << 1 | 1),
		byte(pts >> 7),
		byte((pts&0x7F)<<1 | 1),
	}
}
