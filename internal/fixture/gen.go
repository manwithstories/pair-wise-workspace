// Package fixture generates synthetic MPEG-TS multiplexes with
// controlled defects: null packet drops, continuity counter jumps,
// duplicate packets, scrambled packets and PAT/PMT version toggling
// including PMT PID remapping and stream type changes.
package fixture

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"tsprobe/internal/psi"
)

const rate = 3400 // packets per second

// Options controls the generated multiplex.
type Options struct {
	Out         string
	Programs    int
	Seconds     int
	Drop        float64 // null packet drop probability
	CCJumps     int     // injected CC jumps on program 1 video
	PATVersions int     // PAT versions cycled through
	PMTVersions int     // PMT versions cycled through per program
}

// Summary reports what was generated, for cross-checking scan output.
type Summary struct {
	Packets      int64
	DroppedNulls int64
	CCJumps      int
	ExpectedLost int64
	Dups         int64
	Scrambled    int64
	PATChangeMS  []int64
	PMTChangeMS  []int64
}

func videoPID(prog int) uint16 { return uint16(0x200 + prog*0x10) }
func audioPID(prog int) uint16 { return videoPID(prog) + 1 }
func privPID(prog int) uint16  { return videoPID(prog) + 2 }

func pmtPID(prog, patVersion int) uint16 {
	if prog == 2 && patVersion >= 1 {
		return 0x122 // PMT PID remapping introduced by PAT version 1
	}
	return uint16(0x100 + prog)
}

// changeTimes spreads n-1 version changes over the duration, snapped
// to 4 second boundaries so time based cuts and table cuts coincide.
func changeTimes(n, seconds int) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for j := 1; j < n; j++ {
		ms := int64(math.Round(float64(seconds*1000*j)/float64(n)/4000)) * 4000
		if ms <= 0 || ms >= int64(seconds*1000) || seen[ms] {
			continue
		}
		seen[ms] = true
		out = append(out, ms)
	}
	return out
}

func versionAt(times []int64, ms int64) int {
	v := 0
	for _, t := range times {
		if ms >= t {
			v++
		}
	}
	return v
}

// Generate writes the multiplex and returns its defect summary.
func Generate(opt Options) (*Summary, error) {
	if opt.Programs < 1 {
		opt.Programs = 1
	}
	if opt.Seconds < 1 {
		opt.Seconds = 1
	}
	if err := os.MkdirAll(filepath.Dir(opt.Out), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(opt.Out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()

	rng := rand.New(rand.NewSource(1))
	total := opt.Seconds * rate
	dupAt := map[int]bool{}
	for len(dupAt) < 4 {
		dupAt[340+rng.Intn(total-680)] = true
	}
	scrAt := map[int]bool{}
	for len(scrAt) < 30 {
		scrAt[340+rng.Intn(total-680)] = true
	}

	sum := &Summary{PATChangeMS: changeTimes(opt.PATVersions, opt.Seconds),
		PMTChangeMS: changeTimes(opt.PMTVersions, opt.Seconds)}
	m := &muxer{w: w, cc: map[uint16]uint8{}}

	type cstream struct {
		pid uint16
		sid byte
	}
	var rot []cstream
	for p := 1; p <= opt.Programs; p++ {
		for i := 0; i < 4; i++ {
			rot = append(rot, cstream{videoPID(p), 0xE0})
		}
		for i := 0; i < 2; i++ {
			rot = append(rot, cstream{audioPID(p), 0xC0})
		}
		if p == 1 {
			rot = append(rot, cstream{privPID(p), 0xBD})
		}
	}

	rotIdx := 0
	video1Content := 0
	jumpsLeft := opt.CCJumps
	for s := 0; s < total; s++ {
		ms := int64(s) * 1000 / rate
		patV := versionAt(sum.PATChangeMS, ms)
		pmtV := versionAt(sum.PMTChangeMS, ms)
		if s%340 == 0 { // tables every 100 ms
			m.writeSection(0, patSection(opt.Programs, patV))
			for p := 1; p <= opt.Programs; p++ {
				m.writeSection(pmtPID(p, patV), pmtSection(p, pmtV))
			}
		}
		if s%136 == 0 { // PCR every 40 ms on each program video PID
			for p := 1; p <= opt.Programs; p++ {
				pcr := int64(s) * 90000 / rate
				var payload []byte
				if s%680 == 0 { // PUSI + PTS every 200 ms
					payload = pesPayload(0xE0, pcr, true)
				}
				m.writePacket(videoPID(p), s%680 == 0, pcr, payload, false)
			}
		} else if s%1360 != 0 {
			cs := rot[rotIdx%len(rot)]
			rotIdx++
			pid := cs.pid
			if pid == videoPID(1) {
				video1Content++
				if jumpsLeft > 0 && video1Content%1300 == 500 {
					skip := int64(sum.CCJumps%3) + 1
					m.cc[pid] = (m.cc[pid] + uint8(skip)) & 0xF
					sum.ExpectedLost += skip
					sum.CCJumps++
					jumpsLeft--
				}
			}
			pkt := m.writePacket(pid, false, -1, contentPayload(pid), scrAt[s])
			if scrAt[s] {
				sum.Scrambled++
			}
			if dupAt[s] {
				w.Write(pkt)
				m.n++
				sum.Dups++
			}
		}
		if s%1360 == 0 { // audio PTS every 400 ms
			for p := 1; p <= opt.Programs; p++ {
				m.writePacket(audioPID(p), true, -1, pesPayload(0xC0, int64(s)*90000/rate, true), false)
			}
		}
		if s%10 == 0 { // null padding, subject to drop
			if rng.Float64() < opt.Drop {
				sum.DroppedNulls++
			} else {
				m.writePacket(0x1FFF, false, -1, contentPayload(0x1FFF), false)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	sum.Packets = m.n
	return sum, nil
}

type muxer struct {
	w  *bufio.Writer
	cc map[uint16]uint8
	n  int64
}

func (m *muxer) writeSection(pid uint16, sec []byte) {
	pkts := psi.MarshalSection(pid, m.cc[pid], sec)
	m.w.Write(pkts)
	m.cc[pid] = (m.cc[pid] + uint8(len(pkts)/188)) & 0xF
	m.n += int64(len(pkts) / 188)
}

// writePacket emits one 188 byte packet. pcr < 0 means no PCR.
// Packets without payload are adaptation-only and, per spec, do not
// advance the continuity counter.
func (m *muxer) writePacket(pid uint16, pusi bool, pcr int64, payload []byte, scrambled bool) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte(pid >> 8)
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	afc := byte(1)
	switch {
	case len(payload) == 0:
		afc = 2
	case pcr >= 0 || len(payload) < 184:
		afc = 3
	}
	pkt[3] = afc<<4 | m.cc[pid]
	if scrambled {
		pkt[3] |= 0x80
	}
	if afc != 2 {
		m.cc[pid] = (m.cc[pid] + 1) & 0xF
	}
	i := 4
	if afc >= 2 {
		afl := 183 - len(payload)
		pkt[4] = byte(afl)
		for j := 5; j < 5+afl; j++ {
			pkt[j] = 0xFF
		}
		if pcr >= 0 {
			pkt[5] = 0x10
			pkt[6] = byte(pcr >> 25)
			pkt[7] = byte(pcr >> 17)
			pkt[8] = byte(pcr >> 9)
			pkt[9] = byte(pcr >> 1)
			pkt[10] = byte(pcr<<7) | 0x7E
			pkt[11] = 0
		} else if afl > 0 {
			pkt[5] = 0x00
		}
		i = 5 + afl
	}
	copy(pkt[i:], payload)
	m.w.Write(pkt)
	m.n++
	return pkt
}

func contentPayload(pid uint16) []byte {
	b := make([]byte, 184)
	for i := range b {
		b[i] = byte(pid) + byte(i)
	}
	return b
}

func pesPayload(streamID byte, pts90 int64, withPTS bool) []byte {
	b := []byte{0, 0, 1, streamID, 0, 0}
	if withPTS {
		b[5] = 0x08 // PES packet length: 3 flag bytes + 5 PTS bytes
		b = append(b, 0x80, 0x80, 0x05)
		b = append(b, ptsBytes(pts90)...)
	}
	return b
}

func ptsBytes(pts int64) []byte {
	return []byte{
		0x20 | byte(((pts>>30)&7)<<1) | 1,
		byte(pts >> 22),
		byte(((pts>>15)&0x7F)<<1) | 1,
		byte(pts >> 7),
		byte((pts&0x7F)<<1) | 1,
	}
}

func patSection(programs, version int) []byte {
	sl := 9 + 4*programs
	sec := make([]byte, 8+4*programs)
	sec[0] = 0x00
	sec[1] = 0xB0 | byte(sl>>8)
	sec[2] = byte(sl)
	sec[4] = 1 // TS id
	sec[5] = 0xC1 | byte(version<<1)
	for p := 1; p <= programs; p++ {
		off := 8 + 4*(p-1)
		sec[off] = byte(p >> 8)
		sec[off+1] = byte(p)
		pid := pmtPID(p, version)
		sec[off+2] = 0xE0 | byte(pid>>8)
		sec[off+3] = byte(pid)
	}
	return appendCRC(sec)
}

func pmtSection(prog, version int) []byte {
	type st struct {
		typ  byte
		pid  uint16
		desc []byte
	}
	streams := []st{
		{0x1B, videoPID(prog), nil},
	}
	audioType := byte(0x0F)
	if prog == 1 && version >= 1 {
		audioType = 0x04 // same PID, stream type change from version 1 on
	}
	lang := "und"
	switch prog {
	case 1:
		lang = "eng"
	case 2:
		lang = "chi"
	}
	iso := append([]byte{0x0A, 0x04}, []byte(lang)...)
	iso = append(iso, 0x00)
	if prog == 2 {
		streams = append(streams, st{0x04, audioPID(prog), iso})
	} else {
		streams = append(streams, st{audioType, audioPID(prog), iso})
	}
	if prog == 1 {
		streams = append(streams, st{0x06, privPID(prog), []byte{0x05, 0x04, 'K', 'L', 'V', 'A'}})
	}
	var entries []byte
	for _, s := range streams {
		entries = append(entries, s.typ, 0xE0|byte(s.pid>>8), byte(s.pid),
			0xF0|byte(len(s.desc)>>8), byte(len(s.desc)))
		entries = append(entries, s.desc...)
	}
	sl := 9 + len(entries) + 4 // +4 for the CRC
	sec := make([]byte, 12)
	sec[0] = 0x02
	sec[1] = 0xB0 | byte(sl>>8)
	sec[2] = byte(sl)
	sec[3] = byte(prog >> 8)
	sec[4] = byte(prog)
	sec[5] = 0xC1 | byte(version<<1)
	pcr := videoPID(prog)
	sec[8] = 0xE0 | byte(pcr>>8)
	sec[9] = byte(pcr)
	sec[10] = 0xF0
	sec = append(sec, entries...)
	return appendCRC(sec)
}

func appendCRC(sec []byte) []byte {
	c := psi.CRC32(sec)
	return append(sec, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
}

func (s *Summary) String() string {
	return fmt.Sprintf("packets=%d dropped_nulls=%d cc_jumps=%d expected_lost=%d dups=%d scrambled=%d pat_changes_ms=%v pmt_changes_ms=%v",
		s.Packets, s.DroppedNulls, s.CCJumps, s.ExpectedLost, s.Dups, s.Scrambled, s.PATChangeMS, s.PMTChangeMS)
}
