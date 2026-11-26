package network

import (
	"fmt"
	"math"
	"math/rand"
)

// Profiles supported by the terminal demo and generated fixtures.
const (
	ProfileNormal  = "normal"
	ProfileBurst   = "burst"
	ProfileCorrupt = "corrupt"
)

// Impairment kinds emitted by the deterministic network trajectory.
const (
	ImpDuplicate     = "duplicate"
	ImpReorder       = "reorder"
	ImpWrap          = "sequence_wrap"
	ImpTruncated     = "truncated_packet"
	ImpConflict      = "conflict_payload"
	ImpBurstLoss     = "burst_loss"
	ImpRetransmit    = "retransmission"
	ImpBandwidthDrop = "bandwidth_drop"
)

// Condition is one point on the bandwidth/latency trajectory.
type Condition struct {
	AtMS          int `json:"at_ms"`
	BandwidthKbps int `json:"bandwidth_kbps"`
	RTTMS         int `json:"rtt_ms"`
	JitterMS      int `json:"jitter_ms"`
}

// Impairment is a deterministic event in a generated sample trajectory.
type Impairment struct {
	Kind        string `json:"kind"`
	PacketIndex int    `json:"packet_index"`
	Length      int    `json:"length,omitempty"`
	AtMS        int    `json:"at_ms"`
	DelayMS     int    `json:"delay_ms,omitempty"`
	Repair      bool   `json:"repair,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// Trace is the complete reproducible network plan for one demo run.
type Trace struct {
	Profile          string       `json:"profile"`
	Seed             int64        `json:"seed"`
	PacketCount      int          `json:"packet_count"`
	VideoCount       int          `json:"video_count"`
	PacketIntervalMS int          `json:"packet_interval_ms"`
	StartSequence    uint16       `json:"start_sequence"`
	AudioSSRC        uint32       `json:"audio_ssrc"`
	VideoSSRC        uint32       `json:"video_ssrc"`
	JitterMS         []int        `json:"jitter_ms"`
	Conditions       []Condition  `json:"conditions"`
	Impairments      []Impairment `json:"impairments"`
}

// Generate creates a seed-stable weak-network trajectory and anomaly plan.
func Generate(profile string, seed int64) (*Trace, error) {
	switch profile {
	case ProfileNormal, ProfileBurst, ProfileCorrupt:
	default:
		return nil, fmt.Errorf("network: unknown profile %q", profile)
	}

	t := &Trace{
		Profile: profile, Seed: seed, PacketCount: 48, VideoCount: 48,
		PacketIntervalMS: 20, StartSequence: 65520,
		AudioSSRC: 0xA0D10001, VideoSSRC: 0xB1DE0002,
	}
	if profile == ProfileCorrupt {
		t.PacketCount = 32
		t.VideoCount = 32
	}
	t.JitterMS = deterministicJitter(profile, seed, t.PacketCount)
	t.Conditions = defaultConditions(profile)
	t.Impairments = impairmentsFor(profile)
	return t, nil
}

func deterministicJitter(profile string, seed int64, n int) []int {
	r := rand.New(rand.NewSource(seed))
	out := make([]int, n)
	for i := range out {
		base, span := 5, 12
		if profile == ProfileBurst {
			base, span = 7, 18
		}
		if profile == ProfileCorrupt {
			base, span = 4, 10
		}
		out[i] = base + r.Intn(span)
	}
	return out
}

func defaultConditions(profile string) []Condition {
	switch profile {
	case ProfileBurst:
		return []Condition{
			{AtMS: 0, BandwidthKbps: 1800, RTTMS: 24, JitterMS: 8},
			{AtMS: 180, BandwidthKbps: 320, RTTMS: 72, JitterMS: 26},
			{AtMS: 430, BandwidthKbps: 1500, RTTMS: 28, JitterMS: 10},
		}
	case ProfileCorrupt:
		return []Condition{{AtMS: 0, BandwidthKbps: 1200, RTTMS: 30, JitterMS: 12}}
	default:
		return []Condition{
			{AtMS: 0, BandwidthKbps: 1600, RTTMS: 22, JitterMS: 7},
			{AtMS: 500, BandwidthKbps: 900, RTTMS: 38, JitterMS: 14},
			{AtMS: 760, BandwidthKbps: 1500, RTTMS: 24, JitterMS: 8},
		}
	}
}

func impairmentsFor(profile string) []Impairment {
	wrapIndex := 65536 - 65520 // logical index whose sequence becomes 0.
	common := []Impairment{{Kind: ImpWrap, PacketIndex: wrapIndex, AtMS: wrapIndex * 20, Detail: "uint16 sequence wraps to zero"}}
	switch profile {
	case ProfileNormal:
		return append(common,
			Impairment{Kind: ImpDuplicate, PacketIndex: 3, AtMS: 60, Detail: "exact RTP copy"},
			Impairment{Kind: ImpReorder, PacketIndex: 9, AtMS: 180, DelayMS: 34, Detail: "packet 9 follows packet 10"},
			Impairment{Kind: ImpDuplicate, PacketIndex: 27, AtMS: 540},
		)
	case ProfileBurst:
		events := append(common,
			Impairment{Kind: ImpBandwidthDrop, PacketIndex: 9, AtMS: 180, Detail: "1800->320 kbps"},
			Impairment{Kind: ImpReorder, PacketIndex: 7, AtMS: 140, DelayMS: 28},
			Impairment{Kind: ImpDuplicate, PacketIndex: 4, AtMS: 80},
			Impairment{Kind: ImpBurstLoss, PacketIndex: 10, Length: 7, AtMS: 200, Detail: "seven consecutive media packets lost"},
		)
		for i := 10; i < 15; i++ {
			events = append(events, Impairment{
				Kind: ImpRetransmit, PacketIndex: i, AtMS: 200 + (i-10)*20,
				DelayMS: 92, Repair: true, Detail: "valid retransmission copy",
			})
		}
		return events
	default:
		return append(common,
			Impairment{Kind: ImpDuplicate, PacketIndex: 2, AtMS: 40},
			Impairment{Kind: ImpTruncated, PacketIndex: 6, AtMS: 120, Detail: "RTP header/payload truncated"},
			Impairment{Kind: ImpConflict, PacketIndex: 11, AtMS: 220, Detail: "same sequence with different payload"},
		)
	}
}

// ConditionAt interpolates the bandwidth trajectory at millisecond now.
func (t *Trace) ConditionAt(nowMS int) Condition {
	if len(t.Conditions) == 0 {
		return Condition{AtMS: nowMS, BandwidthKbps: 1200, RTTMS: 30, JitterMS: 10}
	}
	cur := t.Conditions[0]
	for _, c := range t.Conditions {
		if c.AtMS <= nowMS {
			cur = c
		} else {
			break
		}
	}
	return cur
}

// WaitWindowMS derives an adaptive playout window from bandwidth/RTT/jitter.
func WaitWindowMS(c Condition) int {
	serialMS := 2500 / math.Max(64, float64(c.BandwidthKbps))
	wait := 16 + c.JitterMS/3 + c.RTTMS/6 + int(math.Round(serialMS))
	if wait < 35 {
		return 35
	}
	if wait > 80 {
		return 80
	}
	return wait
}

// Has reports whether an impairment kind exists for packet index.
func (t *Trace) Has(kind string, index int) bool {
	for _, im := range t.Impairments {
		if im.Kind == kind && im.PacketIndex == index {
			return true
		}
	}
	return false
}
