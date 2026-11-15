package fixture

import (
	"math/rand"

	"github.com/pion/rtp"
)

// ImpairOptions controls deterministic packet damage.
type ImpairOptions struct {
	Loss      float64
	Reorder   float64
	Duplicate float64
	Seed      int64
}

// Apply returns the same benign or damaged wire sequence for every transport.
func Apply(packets []Packet, opts ImpairOptions) []Packet {
	rng := rand.New(rand.NewSource(opts.Seed))
	out := make([]Packet, 0, len(packets))
	for i, packet := range packets {
		if i > 0 && rng.Float64() < opts.Loss {
			continue
		}
		out = append(out, packet)
		if rng.Float64() < opts.Duplicate {
			out = append(out, clonePacket(packet))
		}
	}
	for i := 1; i+1 < len(out); i++ {
		if rng.Float64() < opts.Reorder {
			out[i], out[i+1] = out[i+1], out[i]
			i++
		}
	}
	return out
}

func clonePacket(p Packet) Packet {
	clone := p
	clone.RTP.Payload = append([]byte(nil), p.RTP.Payload...)
	clone.RTP.Header.Extensions = append([]rtp.Extension(nil), p.RTP.Header.Extensions...)
	return clone
}
