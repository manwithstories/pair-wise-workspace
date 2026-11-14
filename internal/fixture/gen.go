// Package fixture synthesizes deterministic multi-program MPEG-TS streams
// with injected packet loss and continuity-counter jumps.
package fixture

import (
	"io"
	"math/rand/v2"

	"tsprobe/internal/tspkt"
)

// Rate constants (packets per second).
const (
	videoPerSec = 1056 // per program
	audioPerSec = 933  // per audio track
	AudioTracks = 2    // per program
	nullPerSec  = 758
	psiPerSec   = 2 // PSI emission events; each emits PAT + all PMTs
)

// Params controls stream synthesis.
type Params struct {
	Seed         uint64
	Programs     int
	Seconds      int
	PATVersions  int
	PMTVersions  int
	InjectLoss   int
	InjectCCJump int
	Wrap192      bool
}

func (p Params) withDefaults() Params {
	if p.Programs <= 0 {
		p.Programs = 3
	}
	if p.Seconds <= 0 {
		p.Seconds = 30
	}
	if p.PATVersions <= 0 {
		p.PATVersions = 4
	}
	if p.PMTVersions <= 0 {
		p.PMTVersions = 6
	}
	return p
}

// VideoPID returns the video ES PID of program index p.
func VideoPID(p int) uint16 { return 0x0200 + uint16(p)*0x10 }

// AudioPID returns the audio ES PID of track t in program index p.
func AudioPID(p, t int) uint16 { return 0x0201 + uint16(p)*0x10 + uint16(t) }

// PMTPID returns the PMT PID of program index p.
func PMTPID(p int) uint16 { return 0x0100 + uint16(p) }

// ProgramNumber returns the program_number of program index p.
func ProgramNumber(p int) uint16 { return uint16(p + 1) }

// Plan holds the deterministic expected counts of a generated stream.
type Plan struct {
	Packets     uint64            `json:"packets"`
	ESPackets   uint64            `json:"es_packets"`
	NullPackets uint64            `json:"null_packets"`
	PSIPackets  uint64            `json:"psi_packets"`
	PerPID      map[uint16]uint64 `json:"per_pid"`
	LostEst     uint64            `json:"lost_packets_est"`
	JumpEvents  uint64            `json:"cc_jump_events"`
	PATChanges  uint64            `json:"pat_version_changes"`
	PMTChanges  map[uint16]uint64 `json:"pmt_version_changes"`
}

// Plan computes the expected counts without writing the stream.
func (p Params) Plan() *Plan {
	plan, _ := Generate(io.Discard, p)
	return plan
}

type esStream struct {
	pid   uint16
	quota int
	rate  float64
	acc   float64
	cc    byte
}

// pickSet returns k distinct ordinals in [0,n), avoiding excluded and a
// zone of plus/minus 20 ordinals around excluded entries.
func pickSet(rng *rand.Rand, n, k int, excluded map[int]bool) map[int]bool {
	out := map[int]bool{}
	for len(out) < k {
		v := int(rng.Uint64() % uint64(n))
		if excluded != nil {
			blocked := false
			for d := -20; d <= 20; d++ {
				if excluded[v+d] {
					blocked = true
					break
				}
			}
			if blocked || out[v] {
				continue
			}
		} else if out[v] {
			continue
		}
		out[v] = true
	}
	return out
}

// Generate writes a synthetic TS stream to w and returns its Plan.
func Generate(w io.Writer, p Params) (*Plan, error) {
	p = p.withDefaults()
	rng := rand.New(rand.NewPCG(p.Seed, p.Seed^0x9E3779B96FF4))

	esTotal := p.Seconds * p.Programs * (videoPerSec + AudioTracks*audioPerSec)
	nullTotal := p.Seconds * nullPerSec
	psiEvents := p.Seconds * psiPerSec
	totalSlots := esTotal + nullTotal + psiEvents

	// Keep losses away from the stream tail so every gap is observable.
	lossSet := pickSet(rng, esTotal-64, p.InjectLoss, nil)
	jumpSet := pickSet(rng, esTotal, p.InjectCCJump, lossSet)

	// PSI emission schedule: psiEvents evenly spaced slots, first at slot 0.
	psiAt := map[int]bool{}
	for i := 0; i < psiEvents; i++ {
		psiAt[i*totalSlots/psiEvents] = true
	}

	var streams []*esStream
	for prog := 0; prog < p.Programs; prog++ {
		streams = append(streams, &esStream{pid: VideoPID(prog), quota: p.Seconds * videoPerSec})
		for t := 0; t < AudioTracks; t++ {
			streams = append(streams, &esStream{pid: AudioPID(prog, t), quota: p.Seconds * audioPerSec})
		}
	}
	for _, s := range streams {
		s.rate = float64(s.quota) / float64(esTotal)
	}

	plan := &Plan{
		PerPID:     map[uint16]uint64{},
		LostEst:    uint64(p.InjectLoss),
		JumpEvents: uint64(p.InjectCCJump),
		PATChanges: uint64(p.PATVersions - 1),
		PMTChanges: map[uint16]uint64{},
	}
	for prog := 0; prog < p.Programs; prog++ {
		plan.PMTChanges[ProgramNumber(prog)] = uint64(p.PMTVersions - 1)
	}

	psiCC := map[uint16]byte{}
	writePkt := func(pkt []byte) error {
		if p.Wrap192 {
			if _, err := w.Write([]byte{0x00, 0x00, 0x00, 0x00}); err != nil {
				return err
			}
		}
		_, err := w.Write(pkt)
		return err
	}
	emitPSI := func(slot int) error {
		frac := float64(slot) / float64(totalSlots)
		patV := int(frac * float64(p.PATVersions))
		if patV > p.PATVersions-1 {
			patV = p.PATVersions - 1
		}
		if err := writePkt(psiPacket(0, psiCC[0], buildPAT(patV, p.Programs))); err != nil {
			return err
		}
		psiCC[0]++
		plan.PerPID[0]++
		plan.PSIPackets++
		for prog := 0; prog < p.Programs; prog++ {
			stagger := float64(prog) / (float64(p.Programs) * float64(p.PMTVersions))
			v := int((frac + stagger) * float64(p.PMTVersions))
			if v > p.PMTVersions-1 {
				v = p.PMTVersions - 1
			}
			pid := PMTPID(prog)
			if err := writePkt(psiPacket(pid, psiCC[pid], buildPMT(prog, v))); err != nil {
				return err
			}
			psiCC[pid]++
			plan.PerPID[pid]++
			plan.PSIPackets++
		}
		return nil
	}

	var esAcc, nullAcc float64
	esRate := float64(esTotal) / float64(totalSlots)
	nullRate := float64(nullTotal) / float64(totalSlots)
	esOrd := 0
	payload := make([]byte, 184)
	writeNull := func() error {
		nullPkt := make([]byte, tspkt.PacketSize)
		nullPkt[0] = tspkt.SyncByte
		nullPkt[1] = 0x1F
		nullPkt[2] = 0xFF
		nullPkt[3] = 0x10
		for i := 4; i < len(nullPkt); i++ {
			nullPkt[i] = 0xFF
		}
		if err := writePkt(nullPkt); err != nil {
			return err
		}
		plan.PerPID[0x1FFF]++
		plan.NullPackets++
		return nil
	}
	for slot := 0; slot < totalSlots; slot++ {
		esAcc += esRate
		nullAcc += nullRate
		switch {
		case psiAt[slot]:
			if err := emitPSI(slot); err != nil {
				return nil, err
			}
		case nullAcc >= esAcc && nullAcc >= 1:
			nullAcc -= 1
			if err := writeNull(); err != nil {
				return nil, err
			}
		case esAcc >= 1:
			esAcc -= 1
			// Pick the stream with the largest accumulator deficit.
			var best *esStream
			for _, s := range streams {
				s.acc += s.rate
				if best == nil || s.acc > best.acc {
					best = s
				}
			}
			best.acc -= 1
			best.cc = (best.cc + 1) & 0x0F
			if lossSet[esOrd] {
				esOrd++
				continue // packet produced by the encoder but lost in transit
			}
			ccVal := best.cc
			if jumpSet[esOrd] {
				ccVal = (ccVal - 3) & 0x0F // backward jump: event without loss
				best.cc = ccVal
			}
			esOrd++
			for i := range payload {
				payload[i] = byte(rng.Uint64())
			}
			pkt := make([]byte, tspkt.PacketSize)
			pkt[0] = tspkt.SyncByte
			pkt[1] = byte(best.pid>>8) & 0x1F
			pkt[2] = byte(best.pid)
			pkt[3] = 0x10 | ccVal
			copy(pkt[4:], payload)
			if err := writePkt(pkt); err != nil {
				return nil, err
			}
			plan.PerPID[best.pid]++
			plan.ESPackets++
		}
	}
	// Pad any null packets lost to float rounding.
	for plan.NullPackets < uint64(nullTotal) {
		if err := writeNull(); err != nil {
			return nil, err
		}
	}
	plan.Packets = plan.ESPackets + plan.NullPackets + plan.PSIPackets
	return plan, nil
}
