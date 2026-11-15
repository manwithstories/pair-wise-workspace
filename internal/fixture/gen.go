// Package fixture generates deterministic synthetic MPEG-TS files.
package fixture

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

const (
	generatorTicks     = 1500 // 15 seconds in 10 ms ticks
	dataPacketsPerTick = 105
	psiPeriodTicks     = 10
)

// Options controls deterministic fixture generation.
type Options struct {
	Out      string
	Programs int
	Switches int
	Drop     float64
	Seed     int64
}

// Generate writes a synthetic TS file matching the requested profile.
func Generate(o Options) error {
	if o.Out == "" || o.Programs < 1 || o.Programs > 32 || o.Switches < 0 || o.Switches > 1000 ||
		o.Drop < 0 || o.Drop > 1 || math.IsNaN(o.Drop) || math.IsInf(o.Drop, 0) {
		return fmt.Errorf("invalid generator options")
	}
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(o.Out)
	if err != nil {
		return err
	}
	defer f.Close()

	programs := defaultPrograms(o.Programs)
	mux := newMux(bufio.NewWriterSize(f, 128*1024), o, programs)
	if err := mux.run(); err != nil {
		return err
	}
	if err := mux.w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

type streamKind int

const (
	videoStream streamKind = iota
	audioStream
)

type streamState struct {
	kind     streamKind
	program  int
	count    int
	ordinal  int
	lastTick int
	dropPos  int
	dropRuns [][2]int
}

type mux struct {
	w         *bufio.Writer
	opts      Options
	programs  []programConfig
	pw        *packetWriter
	rng       *rand.Rand
	streams   []*streamState
	switchAt  []int
	patVer    uint8
	pmtVer    []uint8
	videoNext int
	audioNext int
}

func newMux(w *bufio.Writer, o Options, programs []programConfig) *mux {
	m := &mux{w: w, opts: o, programs: programs, pw: newPacketWriter(w), rng: rand.New(rand.NewSource(o.Seed)), pmtVer: make([]uint8, len(programs))}
	for i := range programs {
		m.streams = append(m.streams,
			&streamState{kind: videoStream, program: i, count: 94500 / len(programs), lastTick: -1},
			&streamState{kind: audioStream, program: i, count: 63000 / len(programs), lastTick: -1},
		)
	}
	m.prepareDrops()
	m.prepareSwitches()
	return m
}

func (m *mux) prepareSwitches() {
	if m.opts.Switches == 0 {
		return
	}
	start := 300
	for i := 0; i < m.opts.Switches; i++ {
		tick := start + i*((generatorTicks-start)/m.opts.Switches)
		if tick >= generatorTicks {
			tick = generatorTicks - 1
		}
		m.switchAt = append(m.switchAt, tick)
	}
}

func (m *mux) prepareDrops() {
	bursts := int(math.Round(m.opts.Drop * 800))
	if m.opts.Drop > 0 && bursts == 0 {
		bursts = 1
	}
	for i := 0; i < bursts; i++ {
		s := m.rng.Intn(len(m.streams))
		c := m.streams[s].count
		if c < 100 {
			continue
		}
		start := 50 + m.rng.Intn(c-100)
		length := 3 + m.rng.Intn(3)
		m.streams[s].dropRuns = append(m.streams[s].dropRuns, [2]int{start, start + length})
	}
	for _, s := range m.streams {
		s.dropRuns = mergeRuns(s.dropRuns)
	}
}

func mergeRuns(runs [][2]int) [][2]int {
	if len(runs) < 2 {
		return runs
	}
	// Small deterministic insertion sort keeps generator free of map ordering.
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j][0] < runs[j-1][0]; j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
	out := [][2]int{runs[0]}
	for _, r := range runs[1:] {
		last := &out[len(out)-1]
		if r[0] <= last[1] {
			if r[1] > last[1] {
				last[1] = r[1]
			}
		} else {
			out = append(out, r)
		}
	}
	return out
}

func (m *mux) run() error {
	for tick := 0; tick < generatorTicks; tick++ {
		m.applySwitch(tick)
		if tick%psiPeriodTicks == 0 {
			if err := m.emitPSI(tick); err != nil {
				return err
			}
		}
		for i := 0; i < dataPacketsPerTick; i++ {
			var s *streamState
			if i%5 < 3 {
				idx := m.videoNext % len(m.programs)
				m.videoNext++
				s = m.streams[idx*2]
			} else {
				idx := m.audioNext % len(m.programs)
				m.audioNext++
				s = m.streams[idx*2+1]
			}
			m.emitStreamPacket(tick, s)
		}
	}
	return nil
}

func (m *mux) applySwitch(tick int) {
	for i, at := range m.switchAt {
		if at != tick {
			continue
		}
		for p := range m.pmtVer {
			m.pmtVer[p]++
		}
		if i == 1 || i == 2 {
			m.patVer++
		}
		if i == 2 && len(m.programs) >= 2 {
			m.programs[0].pmtPID, m.programs[1].pmtPID = m.programs[1].pmtPID, m.programs[0].pmtPID
		}
		prog := i % len(m.programs)
		if m.programs[prog].audioType == 0x0f {
			m.programs[prog].audioType = 0x03
		} else {
			m.programs[prog].audioType = 0x0f
		}
	}
}

func (m *mux) emitPSI(tick int) error {
	if err := m.pw.writeSection(0, buildPAT(m.patVer, true, m.programs)); err != nil {
		return err
	}
	if err := m.pw.writeSection(0x11, buildSDT(0, true, m.programs)); err != nil {
		return err
	}
	for i, p := range m.programs {
		if err := m.pw.writeSection(p.pmtPID, buildPMT(p, m.pmtVer[i], true)); err != nil {
			return err
		}
	}
	if tick == 750 {
		_ = m.pw.writeSection(m.programs[0].pmtPID, buildPMT(m.programs[0], m.pmtVer[0]+7, false))
	}
	return nil
}

func (m *mux) emitStreamPacket(tick int, s *streamState) {
	firstInTick := s.lastTick != tick
	s.lastTick = tick
	pusi := firstInTick && tick%psiPeriodTicks == 0
	streamOrdinal := s.ordinal
	s.ordinal++
	drop := shouldDrop(s)
	pid := m.programs[s.program].videoPID
	if s.kind == audioStream {
		pid = m.programs[s.program].audioPID
	}
	if pusi {
		pesIndex := tick / psiPeriodTicks
		pts := int64(90000 + pesIndex*9000)
		var es []byte
		streamID := uint8(0xe0)
		if s.kind == videoStream {
			key := tick%(psiPeriodTicks*10) == 0
			es = videoES(key, byte(streamOrdinal))
		} else {
			streamID = 0xc0
			es = audioES(byte(streamOrdinal))
		}
		m.pw.write(pid, true, writePES(streamID, pts, es), drop)
		return
	}
	m.pw.write(pid, false, fillContinuation(s, streamOrdinal), drop)
}

func fillContinuation(s *streamState, ordinal int) []byte {
	b := make([]byte, 184)
	x := uint32(ordinal)*0x9e3779b1 + uint32(s.program+1)*0x85ebca6b
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x>>24) | 0x01
	}
	return b
}

func shouldDrop(s *streamState) bool {
	for s.dropPos < len(s.dropRuns) && s.ordinal >= s.dropRuns[s.dropPos][1] {
		s.dropPos++
	}
	return s.dropPos < len(s.dropRuns) && s.ordinal >= s.dropRuns[s.dropPos][0]
}
