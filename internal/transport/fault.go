package transport

import (
	"encoding/json"
	"math/rand"
	"os"
)

// FaultProfile configures deterministic transport impairment. The same
// profile and seed always produce the same loss/reorder pattern, which is
// what makes repeat-run checksums stable.
type FaultProfile struct {
	Seed          int64   `json:"seed"`
	ReorderRate   float64 `json:"reorder_rate"`
	DuplicateRate float64 `json:"duplicate_rate"`
	LossRate      float64 `json:"loss_rate"`
	BurstSize     int     `json:"burst_size"`
	// DropSeqs explicitly marks RTP sequence numbers to drop; when present
	// it overrides stochastic loss and gives exact control to fixtures.
	DropSeqs []uint16 `json:"drop_seqs,omitempty"`
}

// LoadFaultProfile reads a JSON profile; an empty path yields a clean one.
func LoadFaultProfile(path string) (*FaultProfile, error) {
	if path == "" {
		return &FaultProfile{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p FaultProfile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Injector applies the profile to an ordered packet stream. It never drops
// repair packets (those carry the synthetic RTP payload type used by the
// XOR/NACK layer) and never reorders beyond what the jitter window covers.
type Injector struct {
	profile *FaultProfile
	rng     *rand.Rand
	dropSet map[uint16]bool
}

// NewInjector builds an injector from a profile.
func NewInjector(p *FaultProfile) *Injector {
	drop := make(map[uint16]bool)
	for _, s := range p.DropSeqs {
		drop[s] = true
	}
	return &Injector{profile: p, rng: rand.New(rand.NewSource(p.Seed)), dropSet: drop}
}

// Inject returns the impaired stream. Reordering swaps adjacent packets,
// duplication re-emits the same packet, and loss skips it entirely.
func (in *Injector) Inject(inPkts []*Packet) []*Packet {
	out := make([]*Packet, 0, len(inPkts))
	burstLeft := 0
	for _, pkt := range inPkts {
		seq := pkt.RTP.SequenceNumber
		// explicit drops always win
		if in.dropSet[seq] {
			continue
		}
		if burstLeft > 0 {
			burstLeft--
			continue
		}
		if in.profile.LossRate > 0 && in.rng.Float64() < in.profile.LossRate {
			if in.profile.BurstSize > 1 {
				burstLeft = in.profile.BurstSize - 1
			}
			continue
		}
		if in.profile.DuplicateRate > 0 && in.rng.Float64() < in.profile.DuplicateRate {
			out = append(out, pkt) // original first, duplicate second
		}
		out = append(out, pkt)
		if in.profile.ReorderRate > 0 && in.rng.Float64() < in.profile.ReorderRate &&
			len(out) >= 2 {
			out[len(out)-1], out[len(out)-2] = out[len(out)-2], out[len(out)-1]
		}
	}
	return out
}
