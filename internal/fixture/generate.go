// Package fixture generates deterministic local CMAF/fMP4 samples and faults.
package fixture

import (
	"fmt"

	"github.com/pairwise/relay/internal/isobmff"
	"github.com/pairwise/relay/internal/pipeline"
	"github.com/pairwise/relay/internal/transport"
)

const streamPrefix = "relay-seed"

type ladder struct {
	id         string
	timescale  uint32
	sampleDur  uint32
	bandwidth  uint32
	average    uint32
	resolution string
	codecs     string
	sizeBase   uint32
}

var ladders = []ladder{
	{id: "high", timescale: 90000, sampleDur: 30000, bandwidth: 5_200_000, average: 4_600_000, resolution: "1920x1080", codecs: "avc1.640028,mp4a.40.2", sizeBase: 220},
	{id: "mid", timescale: 48000, sampleDur: 16000, bandwidth: 2_100_000, average: 1_800_000, resolution: "1280x720", codecs: "avc1.4d401f,mp4a.40.2", sizeBase: 140},
	{id: "low", timescale: 44100, sampleDur: 14700, bandwidth: 850_000, average: 720_000, resolution: "640x360", codecs: "avc1.42e01e,mp4a.40.2", sizeBase: 80},
}

// Config controls deterministic sample generation.
type Config struct {
	Seed       int64
	Rungs      int
	Segments   int
	FaultSuite bool
}

// Generate builds the complete in-memory demo input. No external files or
// random sources are used beyond the seed-derived size pattern.
func Generate(cfg Config) pipeline.Input {
	if cfg.Rungs <= 0 {
		cfg.Rungs = 3
	}
	if cfg.Segments <= 0 {
		cfg.Segments = 24
	}
	streamID := fmt.Sprintf("%s-%d", streamPrefix, cfg.Seed)
	in := pipeline.Input{
		Seed:        cfg.Seed,
		Segments:    cfg.Segments,
		Rungs:       cfg.Rungs,
		Duration90k: pipeline.Duration90k,
		StreamID:    streamID,
		Window:      8,
		Bandwidth: transport.BandwidthTrace{
			{AtMS: 0, Kbps: 2400},
			{AtMS: 35, Kbps: 512},
			{AtMS: 90, Kbps: 180},
			{AtMS: 160, Kbps: 1200},
		},
		Budget: transport.RecoveryBudget{TotalMillis: 900, RTTMillis: 18, MaxAttempts: 2},
	}

	for ri := 0; ri < cfg.Rungs; ri++ {
		lt := ladders[ri%len(ladders)]
		if cfg.Rungs > len(ladders) {
			lt.id = fmt.Sprintf("rung-%d", ri+1)
		}
		rung := pipeline.RungInput{
			ID:         lt.id,
			SourceNode: "edge-a",
			Timescale:  lt.timescale,
			Bandwidth:  lt.bandwidth,
			Average:    lt.average,
			Resolution: lt.resolution,
			Codecs:     lt.codecs,
			FrameRate:  30,
			Intact:     make(map[uint64][]byte, cfg.Segments),
			Unavailable: map[uint64]bool{
				20: true, // explicit unrecoverable common section
			},
		}
		fragments := make(map[uint64][]byte, cfg.Segments)
		for seg := uint64(1); seg <= uint64(cfg.Segments); seg++ {
			raw := buildSegment(lt, cfg.Seed, seg)
			fragments[seg] = raw
			rung.Intact[seg] = raw
		}
		rung.Packets = buildArrival(streamID, lt.id, fragments)
		in.RungInputs = append(in.RungInputs, rung)
	}

	if cfg.FaultSuite {
		in.Damage = damageSamples()
		in.ForgeCache = []pipeline.ForgeCacheEntry{forgedCacheEntry(streamID)}
	}
	return in
}

func buildSegment(lt ladder, seed int64, seg uint64) []byte {
	trackDuration := uint64(lt.sampleDur) * 6
	tfdt := (seg - 1) * trackDuration
	if seg == 14 {
		// Repairable one-segment tfdt fallback.
		tfdt = (seg - 2) * trackDuration
	}
	mfhd := uint32(seg)
	if seg >= 6 {
		// Missing one mfhd sequence number after sequence 5.
		mfhd = uint32(seg + 1)
	}
	samples := make([]isobmff.SampleConfig, 0, 6)
	for i := 0; i < 6; i++ {
		size := lt.sizeBase + uint32((seed+int64(seg)+int64(i))%9)*4
		samples = append(samples, isobmff.SampleConfig{Duration: lt.sampleDur, Size: size})
	}
	raw, err := isobmff.BuildFragment(isobmff.FragmentConfig{
		Sequence: mfhd,
		TFDT:     tfdt,
		Samples:  samples,
	})
	if err != nil {
		panic(fmt.Sprintf("deterministic fixture build failed: %v", err))
	}
	return raw
}

func buildArrival(streamID, rung string, fragments map[uint64][]byte) []transport.Packet {
	n := uint64(len(fragments))
	dropped := map[uint64]bool{10: true, 11: true, 20: true}
	var packets []transport.Packet
	add := func(seq uint64, retrans bool) {
		packets = append(packets, transport.Packet{
			ID:             transport.PacketID{Stream: streamID, Rendition: rung, Sequence: seq},
			SourceNode:     "edge-a",
			Data:           fragments[seq],
			Bytes:          int64(len(fragments[seq])),
			Retransmission: retrans,
		})
	}
	for seq := uint64(1); seq <= n; seq++ {
		if dropped[seq] {
			continue
		}
		switch seq {
		case 3:
			// Delivered together with seq=4 below to create reordering.
			continue
		case 4:
			add(4, false)
			add(3, false)
			add(3, true) // duplicate after ordered drain
		case 5:
			add(5, false)
			add(5, true) // immediate duplicate
		default:
			add(seq, false)
		}
	}
	return packets
}
