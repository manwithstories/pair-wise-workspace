// Package fixture synthesises everything the demo and tests need: HLS and
// DASH manifests, jittery RTP packet streams and a small but structurally
// valid fragmented-free MP4 with keyframes. No external asset is ever read.
package fixture

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// Segment describes one generated media segment before it is rendered.
type Segment struct {
	Track   string
	Seq     uint64
	StartNS int64
	DurNS   int64
	Payload []byte
	Hash    string
}

// Plan is a fully generated camera/track sample.
type Plan struct {
	Camera   string
	Segments []Segment
	// Packets is the reordered RTP observation stream (out of order).
	Packets  []RPTPacket
	TargetNS int64
}

// RPTPacket is one generated observation with its arrival index.
type RPTPacket struct {
	Seq       uint16
	Extended  uint64
	Timestamp uint32
	Track     string
	Size      int
	Arrival   int
}

// GenConfig controls the generated sample.
type GenConfig struct {
	Seed         int64
	Camera       string
	Segments     int
	SegmentDurNS int64
	PPT          int // RTP packets per second of media
	// Faults selects the injected defects.
	Faults FaultSet
}

// FaultSet names every injectable defect.
type FaultSet struct {
	Reorder  bool
	Truncate bool
	Drift    bool
	Drop     bool
	Wrap     bool
	Late     bool
}

// ParseFaults turns "reorder,truncate,drift" into a FaultSet.
func ParseFaults(s string) FaultSet {
	var f FaultSet
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(strings.ToLower(part)) {
		case "reorder":
			f.Reorder = true
		case "truncate":
			f.Truncate = true
		case "drift":
			f.Drift = true
		case "drop":
			f.Drop = true
		case "wrap":
			f.Wrap = true
		case "late":
			f.Late = true
		}
	}
	return f
}

// DefaultGenConfig produces a 3-camera, 12-segment-per-camera sample whose
// aggregate runtime is well under the performance budget even when scaled.
func DefaultGenConfig(seed int64) GenConfig {
	return GenConfig{
		Seed:         seed,
		Camera:       "cam0",
		Segments:     12,
		SegmentDurNS: 4_000_000_000,
		PPT:          50,
	}
}

// Generate builds a deterministic plan for one camera.
func Generate(cfg GenConfig) Plan {
	rng := rand.New(rand.NewSource(cfg.Seed))
	plan := Plan{Camera: cfg.Camera}
	plan.TargetNS = cfg.SegmentDurNS
	for i := 0; i < cfg.Segments; i++ {
		seq := uint64(i)
		start := int64(i) * cfg.SegmentDurNS
		body := make([]byte, 64+rng.Intn(64))
		for k := range body {
			body[k] = byte(rng.Intn(256))
		}
		plan.Segments = append(plan.Segments, Segment{
			Track:   cfg.Camera,
			Seq:     seq,
			StartNS: start,
			DurNS:   cfg.SegmentDurNS,
			Payload: body,
		})
	}
	// Generate RTP observations, then perturb them.
	base := uint32(1_000_000)
	seq := uint16(1)
	var pkts []RPTPacket
	for i := 0; i < cfg.Segments; i++ {
		n := int(cfg.SegmentDurNS / 1_000_000_000 * int64(cfg.PPT))
		if n <= 0 {
			n = 1
		}
		for k := 0; k < n; k++ {
			p := RPTPacket{
				Seq:       seq,
				Extended:  uint64(i)*uint64(n) + uint64(k),
				Timestamp: base + uint32(uint64(k)*90000/uint64(cfg.PPT)),
				Track:     cfg.Camera,
				Size:      1200,
				Arrival:   len(pkts),
			}
			pkts = append(pkts, p)
			seq++
		}
	}
	if cfg.Faults.Reorder {
		// Swap every adjacent pair after the first two packets.
		for i := 2; i+1 < len(pkts); i += 4 {
			pkts[i], pkts[i+1] = pkts[i+1], pkts[i]
		}
	}
	if cfg.Faults.Drift {
		// Audio-style clock runs 300ppm fast: timestamps gain ~0.03%.
		for i := range pkts {
			pkts[i].Timestamp = base + uint32(uint64(pkts[i].Timestamp-base)*10003/10000)
		}
	}
	if cfg.Faults.Wrap {
		for i := range pkts {
			pkts[i].Seq = uint16(uint64(pkts[i].Seq) + (1 << 16) - 1)
		}
	}
	if cfg.Faults.Drop {
		// Drop one in every 40 packets; leave a clear hole.
		var kept []RPTPacket
		for i := range pkts {
			if i%40 == 17 {
				continue
			}
			kept = append(kept, pkts[i])
		}
		pkts = kept
	}
	plan.Packets = pkts
	return plan
}

// ContentHashOf formats a deterministic pseudo SHA-256-shaped digest for a
// generated payload. The pipeline's real hashing lives in provenance.
func ContentHashOf(b []byte) string {
	return fmt.Sprintf("%064x", uint64(len(b))*7919+1)
}

// SortSegments orders a slice by start time then sequence.
func SortSegments(s []Segment) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].StartNS != s[j].StartNS {
			return s[i].StartNS < s[j].StartNS
		}
		return s[i].Seq < s[j].Seq
	})
}
