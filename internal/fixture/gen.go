// Package fixture generates deterministic, impaired RTP video packet streams
// for local integration demos. No external media files are used.
package fixture

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pion/rtp"
)

// Impairment probabilities are applied independently per generated packet.
type Impairment struct {
	Loss      float64
	Duplicate float64
	Reorder   float64
}

// ParseImpairment parses "loss=0.01,dup=0.01,reorder=0.02".
func ParseImpairment(spec string) (Impairment, error) {
	var out Impairment
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}
	for _, field := range strings.Split(spec, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			return out, fmt.Errorf("impairment %q must be key=value", field)
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || parsed < 0 || parsed >= 1 {
			return out, fmt.Errorf("impairment %s must be in [0,1)", key)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "loss":
			out.Loss = parsed
		case "dup", "duplicate":
			out.Duplicate = parsed
		case "reorder":
			out.Reorder = parsed
		default:
			return out, fmt.Errorf("unknown impairment %q", key)
		}
	}
	return out, nil
}

// Options controls the deterministic fixture.
type Options struct {
	Packets        int
	Duration       time.Duration
	Seed           int64
	Impairment     Impairment
	ClockRate      uint32
	PayloadSize    int
	StartSequence  uint16
	StartTimestamp uint32
}

func (o Options) withDefaults() Options {
	if o.Duration <= 0 {
		o.Duration = 6 * time.Second
	}
	if o.ClockRate == 0 {
		o.ClockRate = 90000
	}
	if o.PayloadSize <= 0 {
		o.PayloadSize = 140
	}
	if o.Packets <= 0 {
		o.Packets = 3000
	}
	return o
}

// Scheduled is one RTP write at a time offset from the start of the run.
type Scheduled struct {
	Packet        *rtp.Packet
	At            time.Duration
	OriginalIndex int
	Duplicate     bool
}

// Generated contains the schedule and deterministic counters.
type Generated struct {
	Events     []Scheduled
	Packets    int
	Lost       int
	Duplicated int
	Reordered  int
	StartSeq   uint16
	StartTS    uint32
	ClockRate  uint32
	Interval   time.Duration
}

// Generate creates packets with loss, duplicate copies and delayed (reordered)
// delivery. The same seed and options always produce the same logical counts.
func Generate(options Options) (Generated, error) {
	options = options.withDefaults()
	if options.Packets <= 0 || options.Duration <= 0 {
		return Generated{}, fmt.Errorf("packets and duration must be positive")
	}
	if options.Impairment.Loss < 0 || options.Impairment.Loss >= 1 ||
		options.Impairment.Duplicate < 0 || options.Impairment.Duplicate >= 1 ||
		options.Impairment.Reorder < 0 || options.Impairment.Reorder >= 1 {
		return Generated{}, fmt.Errorf("impairment probabilities must be in [0,1)")
	}
	interval := options.Duration / time.Duration(options.Packets)
	if interval <= 0 {
		return Generated{}, fmt.Errorf("duration too short for packet count")
	}
	tsStep := uint32((uint64(options.ClockRate) * uint64(interval)) / uint64(time.Second))
	if tsStep == 0 {
		tsStep = 1
	}
	rng := rand.New(rand.NewSource(options.Seed))
	events := make([]Scheduled, 0, options.Packets+options.Packets/20)
	generated := Generated{
		Packets:   options.Packets,
		StartSeq:  options.StartSequence,
		StartTS:   options.StartTimestamp,
		ClockRate: options.ClockRate,
		Interval:  interval,
	}

	for i := 0; i < options.Packets; i++ {
		if rng.Float64() < options.Impairment.Loss {
			generated.Lost++
			continue
		}
		reordered := rng.Float64() < options.Impairment.Reorder
		duplicated := rng.Float64() < options.Impairment.Duplicate
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: options.StartSequence + uint16(i),
				Timestamp:      options.StartTimestamp + uint32(i)*tsStep,
				SSRC:           0x11111111,
			},
			Payload: make([]byte, options.PayloadSize),
		}
		for j := range pkt.Payload {
			pkt.Payload[j] = byte((i + j*7) & 0xFF)
		}
		at := time.Duration(i) * interval
		if reordered {
			extra := 2*interval + time.Duration(rng.Int63n(int64(3*interval)+1))
			at += extra
			generated.Reordered++
		}
		events = append(events, Scheduled{Packet: pkt, At: at, OriginalIndex: i})
		if duplicated {
			copyPacket := *pkt
			copyPacket.Payload = append([]byte(nil), pkt.Payload...)
			dupAt := at + 3*interval + time.Duration(rng.Int63n(int64(5*interval)+1))
			events = append(events, Scheduled{Packet: &copyPacket, At: dupAt, OriginalIndex: i, Duplicate: true})
			generated.Duplicated++
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].At == events[j].At {
			return events[i].OriginalIndex < events[j].OriginalIndex
		}
		return events[i].At < events[j].At
	})
	generated.Events = events
	return generated, nil
}
