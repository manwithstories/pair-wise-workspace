// Package synth generates deterministic MPEG-TS streams with known truth.
package synth

import (
	"bufio"
	"errors"
	"io"
)

type Config struct {
	Programs   int
	Packets    int
	Bumps      int
	LossBursts int
	Dups       int
	Seed       int64
}

type Manifest struct {
	Packets           int            `json:"packets"`
	Bytes             int64          `json:"bytes"`
	PATChanges        int            `json:"pat_version_changes"`
	PMTVersionChanges map[uint16]int `json:"pmt_version_changes"`
	Dropped           int            `json:"lost_packets"`
	Duplicates        int            `json:"cc_duplicates"`
	CCJumps           int            `json:"cc_jumps"`
}

func (c Config) Validate() error {
	switch {
	case c.Programs < 1 || c.Programs > 16:
		return errors.New("--programs must be between 1 and 16")
	case c.Packets < 1:
		return errors.New("--packets must be positive")
	case c.Bumps < 0 || c.Bumps > 64:
		return errors.New("--bumps must be between 0 and 64")
	case c.LossBursts < 0 || c.LossBursts > 32:
		return errors.New("--loss-bursts must be between 0 and 32")
	case c.Dups < 0 || c.Dups > 1000:
		return errors.New("--dup must be between 0 and 1000")
	}
	return nil
}

type generator struct {
	cfg         Config
	programs    []programDef
	streams     []streamDef
	pw          *packetWriter
	patVersion  uint8
	pmtVersion  map[uint16]uint8
	pmtRevision map[uint16]uint8
	losses      []lossWindow
	dropTotal   int
	dropped     int
	dupsLeft    int
	duplicated  int
	esSequence  uint64
	esCursor    int
}

func Generate(w io.Writer, cfg Config) (*Manifest, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(w, 256*1024)
	g := &generator{
		cfg: cfg, programs: makePrograms(cfg.Programs), pw: newPacketWriter(bw),
		patVersion: 29, pmtVersion: make(map[uint16]uint8), pmtRevision: make(map[uint16]uint8),
		dupsLeft: cfg.Dups,
	}
	positions, pmtPlan, patChanges := makePlan(cfg)
	for i, p := range g.programs {
		g.pmtVersion[p.Number] = uint8((30 + i) & 0x1f)
		g.pmtRevision[p.Number] = 1
		g.streams = append(g.streams, p.Streams...)
	}
	g.losses = makeLossPlan(cfg, positions)
	for _, lw := range g.losses {
		for _, n := range lw.drops {
			g.dropTotal += n
		}
	}
	changeTables := patChanges + len(g.programs) + 1
	for _, pms := range pmtPlan {
		changeTables += len(pms)
	}
	normalSlots := cfg.Packets - changeTables + g.dropTotal - cfg.Dups
	if normalSlots < 100 {
		normalSlots = cfg.Packets
	}

	if err := g.writePAT(); err != nil {
		return nil, err
	}
	for _, p := range g.programs {
		if err := g.writePMT(p); err != nil {
			return nil, err
		}
	}

	nextEpoch := 0
	for slot := 0; slot < normalSlots; slot++ {
		if nextEpoch < len(positions) && slot == positions[nextEpoch] {
			if err := g.applyEpoch(nextEpoch, pmtPlan[nextEpoch], nextEpoch < patChanges); err != nil {
				return nil, err
			}
			nextEpoch++
		}
		if slot%10 != 0 {
			if g.tryDrop(slot) {
				continue
			}
			def := g.streams[g.esCursor%len(g.streams)]
			g.esCursor++
			g.esSequence++
			pes := makePES(def, g.esSequence^uint64(cfg.Seed))
			raw := g.pw.payloadPacket(def.PID, true, pes)
			if err := g.pw.put(raw); err != nil {
				return nil, err
			}
			if g.dupsLeft > 0 && def.isVideo() && slot > normalSlots/10 {
				if err := g.pw.put(raw); err != nil {
					return nil, err
				}
				g.dupsLeft--
				g.duplicated++
			}
			continue
		}
		if err := g.writeControl(slot); err != nil {
			return nil, err
		}
	}
	for g.pw.written < int64(cfg.Packets) {
		if err := g.pw.put(g.pw.nullPacket()); err != nil {
			return nil, err
		}
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	truth := pmtChangeCounts(cfg, pmtPlan)
	return &Manifest{
		Packets: int(g.pw.written), Bytes: g.pw.bytes, PATChanges: patChanges,
		PMTVersionChanges: truth, Dropped: g.dropped, Duplicates: g.duplicated,
		CCJumps: countJumpPIDs(g.losses),
	}, nil
}

func (g *generator) applyEpoch(epoch int, pmtPrograms []int, patChange bool) error {
	if patChange {
		g.patVersion = (g.patVersion + 1) & 0x1f
		if err := g.writePAT(); err != nil {
			return err
		}
	}
	for _, index := range pmtPrograms {
		if index < 0 || index >= len(g.programs) {
			continue
		}
		p := g.programs[index]
		g.pmtVersion[p.Number] = (g.pmtVersion[p.Number] + 1) & 0x1f
		g.pmtRevision[p.Number]++
		if err := g.writePMT(p); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) writeControl(slot int) error {
	control := slot / 10
	switch control % 10 {
	case 0:
		return g.writePAT()
	case 1, 2, 3:
		index := control%10 - 1
		if index < len(g.programs) {
			return g.writePMT(g.programs[index])
		}
	}
	return g.pw.put(g.pw.nullPacket())
}

func (g *generator) writePAT() error {
	sections := buildPAT(g.programs, g.patVersion)
	payload := make([]byte, 1, 1+len(sections)*24)
	payload[0] = 0
	for _, sec := range sections {
		payload = append(payload, sec...)
	}
	return g.pw.put(g.pw.payloadPacket(0, true, payload))
}

func (g *generator) writePMT(p programDef) error {
	sec := buildPMT(p, g.pmtVersion[p.Number], g.pmtRevision[p.Number])
	payload := append([]byte{0}, sec...)
	return g.pw.put(g.pw.payloadPacket(p.PMTPID, true, payload))
}

func (g *generator) tryDrop(slot int) bool {
	for i := range g.losses {
		lw := &g.losses[i]
		if slot < lw.start || slot > lw.end {
			continue
		}
		def := g.streams[g.esCursor%len(g.streams)]
		count := lw.drops[def.PID]
		if count <= 0 {
			continue
		}
		lw.drops[def.PID] = count - 1
		g.pw.cc[def.PID] = (g.pw.cc[def.PID] + 1) & 0x0f
		g.esCursor++
		g.esSequence++
		g.dropped++
		return true
	}
	return false
}
