package fixtures

import (
	"math/rand"

	"tsaudit/internal/ts"
)

const (
	patPID       = 0x0000
	p1PMTPID     = 0x0100
	p2PMTPID     = 0x0200
	p2PMTRemap   = 0x0210
	p1VideoPID   = 0x0101
	p1AudioPID   = 0x0102
	p2VideoPID   = 0x0201
	p2AudioPID   = 0x0202
	p2AudioRemap = 0x0212
	zeroPID      = 0x01f1
	strayPID     = 0x0300
	ghostProgram = 9000
	ghostPMTPID  = 0x01f0
)

type packetSpec struct {
	PID     uint16
	PUSI    bool
	CC      uint8
	Payload []byte
	Adapt   *ts.Adaptation
	Data    bool
}

type builder struct {
	cfg        Config
	cc         map[uint16]uint8
	specs      []packetSpec
	sectionSeq int
	corrupt    int
}

func buildSpecs(cfg Config) ([]packetSpec, *Truth, int, int) {
	b := &builder{cfg: cfg, cc: make(map[uint16]uint8)}
	rng := rand.New(rand.NewSource(cfg.Seed))
	ticks := cfg.DurationMS * 4
	for tick := 0; tick < ticks; tick++ {
		ms := float64(tick) * 0.25
		if tick%1600 == 0 {
			b.emitTables(ms)
		}
		if tick%160 == 0 {
			program := (tick/160)%cfg.Programs + 1
			b.emitPCR(videoPID(program), ms)
		}
		pid := uint16(0)
		if tick%97 == 0 {
			pid = strayPID
		} else {
			pids := activeDataPIDs(cfg.Programs, pmtVersionAt(ms, cfg))
			pid = pids[tick%len(pids)]
		}
		b.emitData(pid, tick)
	}

	jumpPIDs := chooseJumpPIDs(cfg)
	specs, dropped := injectDrops(b.specs, cfg.Drop, jumpPIDs, rng)
	jumps := injectCCJumps(specs, cfg.CCJump, jumpPIDs, rng)
	truth := &Truth{
		Version: 1, Seed: cfg.Seed, Programs: cfg.Programs, DurationMS: cfg.DurationMS,
		PAT:        TableTruth{VersionChanges: max(cfg.PATVersions-1, 0)},
		Continuity: ContinuityTruth{LossPackets: dropped, CCJumps: jumps},
		CRCErrors:  b.corrupt,
		OrphanPMTs: []OrphanPMTTruth{{Program: ghostProgram, PID: ghostPMTPID}},
		Injected: InjectionTruth{Drop: cfg.Drop, CCJump: cfg.CCJump, CRCErrors: cfg.CRCErrors,
			PATVersions: cfg.PATVersions, PMTVersions: cfg.PMTVersions},
	}
	for program := 1; program <= cfg.Programs; program++ {
		truth.PMT = append(truth.PMT, PMTTruth{Program: program, VersionChanges: max(cfg.PMTVersions-1, 0)})
	}
	return specs, truth, dropped, jumps
}

func chooseJumpPIDs(cfg Config) []uint16 {
	pids := []uint16{p1VideoPID, p1AudioPID, p2VideoPID, p2AudioPID, strayPID}
	if cfg.Programs < 2 {
		pids = []uint16{p1VideoPID, p1AudioPID, strayPID}
	}
	if cfg.Drop > 0 && len(pids) > 1 {
		pids = pids[:len(pids)-1]
	}
	n := cfg.CCJump
	if n > len(pids)*4 {
		n = len(pids) * 4
	}
	out := make([]uint16, n)
	for i := range out {
		out[i] = pids[i%len(pids)]
	}
	return out
}

func injectDrops(specs []packetSpec, n int, jumpPIDs []uint16, rng *rand.Rand) ([]packetSpec, int) {
	if n <= 0 {
		return specs, 0
	}
	jump := make(map[uint16]bool)
	for _, pid := range jumpPIDs {
		jump[pid] = true
	}
	last := make(map[uint16]int)
	for i, s := range specs {
		if s.Data {
			last[s.PID] = i
		}
	}
	var candidates []int
	for i, s := range specs {
		if s.Data && !jump[s.PID] && i < last[s.PID] {
			candidates = append(candidates, i)
		}
	}
	drop := make(map[int]bool)
	if len(candidates) < n {
		for i, s := range specs {
			if s.Data && i < last[s.PID] && !drop[i] {
				candidates = append(candidates, i)
			}
		}
	}
	for len(drop) < n && len(candidates) > 0 {
		j := rng.Intn(len(candidates))
		drop[candidates[j]] = true
		candidates = append(candidates[:j], candidates[j+1:]...)
	}
	out := make([]packetSpec, 0, len(specs)-len(drop))
	for i, s := range specs {
		if !drop[i] {
			out = append(out, s)
		}
	}
	return out, len(drop)
}

func injectCCJumps(specs []packetSpec, n int, pids []uint16, rng *rand.Rand) int {
	if n <= 0 {
		return 0
	}
	applied := 0
	for j := 0; j < n; j++ {
		pid := pids[j%len(pids)]
		var idxs []int
		for i, s := range specs {
			if s.PID == pid && s.Data {
				idxs = append(idxs, i)
			}
		}
		if len(idxs) < 4 {
			continue
		}
		start := idxs[len(idxs)/4+rng.Intn(len(idxs)/2)]
		for i := start; i < len(specs); i++ {
			if specs[i].PID == pid {
				specs[i].CC = (specs[i].CC + 13) & 0x0f
			}
		}
		applied++
	}
	return applied
}

func (b *builder) nextCC(pid uint16) uint8 {
	cc := b.cc[pid]
	b.cc[pid] = (cc + 1) & 0x0f
	return cc
}

func (b *builder) emitData(pid uint16, tick int) {
	payload := make([]byte, 184)
	for i := range payload {
		payload[i] = byte((int(pid) + i + tick) & 0xff)
	}
	b.specs = append(b.specs, packetSpec{PID: pid, CC: b.nextCC(pid), Payload: payload, Data: true})
}

func (b *builder) emitPCR(pid uint16, ms float64) {
	base := int64(ms * 90)
	payload := make([]byte, 24)
	for i := range payload {
		payload[i] = byte((i*7 + int(pid)) & 0xff)
	}
	a := &ts.Adaptation{HasPCR: true, PCRBase: base}
	b.specs = append(b.specs, packetSpec{PID: pid, PUSI: true, CC: b.nextCC(pid), Payload: payload, Adapt: a, Data: true})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
