package pipeline

import (
	"fmt"
	"sort"

	"mediaqc/internal/media"
	"mediaqc/internal/network"
	rtpio "mediaqc/internal/rtp"
	"mediaqc/internal/sync"
)

// Config configures one deterministic QC pipeline run.
type Config struct {
	Profile  string
	Codec    string
	Seed     int64
	FailOnce string
}

type wireEvent struct {
	Raw            []byte
	ArrivalMS      int64
	LogicalIndex   int
	SSRC           uint32
	Retransmission bool
}

type scenario struct {
	Trace       *network.Trace
	Codec       string
	AudioTicks  uint64
	StartExtSeq uint64
	Events      []wireEvent
	VideoFrames []sync.Frame
	SendMS      []int
}

func generateScenario(cfg Config) (*scenario, error) {
	tr, err := network.Generate(cfg.Profile, cfg.Seed)
	if err != nil {
		return nil, err
	}
	codec := cfg.Codec
	if codec == "" {
		codec = "opus"
	}
	if codec != "opus" && codec != "aac" {
		return nil, media.New(media.KindMalformed, "pipeline.generateScenario", "unsupported codec "+codec)
	}

	ticks, err := codecTicks(codec)
	if err != nil {
		return nil, err
	}
	sc := &scenario{
		Trace: tr, Codec: codec, AudioTicks: ticks,
		StartExtSeq: uint64(tr.StartSequence),
		SendMS:      make([]int, tr.PacketCount),
	}

	lossStart, lossLen := -1, 0
	retransDelay := map[int]int{}
	reorderIndex := -1
	truncatedIndex := -1
	for _, im := range tr.Impairments {
		switch im.Kind {
		case network.ImpBurstLoss:
			lossStart, lossLen = im.PacketIndex, im.Length
		case network.ImpRetransmit:
			retransDelay[im.PacketIndex] = im.DelayMS
		case network.ImpReorder:
			reorderIndex = im.PacketIndex
		case network.ImpTruncated:
			truncatedIndex = im.PacketIndex
		}
	}

	for i := 0; i < tr.PacketCount; i++ {
		sc.SendMS[i] = i * tr.PacketIntervalMS
		payload, err := audioPayload(codec, i)
		if err != nil {
			return nil, err
		}
		seq := tr.StartSequence + uint16(i)
		ts := uint32(0xfffff000) + uint32(i)*uint32(ticks)
		pt := uint8(111)
		if codec == "aac" {
			pt = 96
		}
		raw, err := rtpio.MakeDatagram(tr.AudioSSRC, seq, ts, pt, true, payload)
		if err != nil {
			return nil, media.Wrap(media.KindInternal, "pipeline.generateScenario", "marshal audio", err)
		}

		switch {
		case i == truncatedIndex:
			sc.Events = append(sc.Events, wireEvent{
				Raw: raw[:4], ArrivalMS: int64(sc.SendMS[i] + tr.JitterMS[i]),
				LogicalIndex: i, SSRC: tr.AudioSSRC,
			})
		case lossStart >= 0 && i >= lossStart && i < lossStart+lossLen:
			if delay, ok := retransDelay[i]; ok {
				sc.Events = append(sc.Events, wireEvent{
					Raw: raw, ArrivalMS: int64(sc.SendMS[i] + delay),
					LogicalIndex: i, SSRC: tr.AudioSSRC, Retransmission: true,
				})
			}
		default:
			arrival := int64(sc.SendMS[i] + tr.JitterMS[i])
			if i == reorderIndex && i+1 < tr.PacketCount {
				arrival = int64((i+1)*tr.PacketIntervalMS + tr.JitterMS[i+1] + 6)
			}
			sc.Events = append(sc.Events, wireEvent{
				Raw: raw, ArrivalMS: arrival, LogicalIndex: i, SSRC: tr.AudioSSRC,
			})
			if tr.Has(network.ImpDuplicate, i) {
				sc.Events = append(sc.Events, wireEvent{
					Raw: raw, ArrivalMS: arrival + 8, LogicalIndex: i, SSRC: tr.AudioSSRC,
				})
			}
			if tr.Has(network.ImpConflict, i) {
				other, err := rtpio.MakeDatagram(tr.AudioSSRC, seq, ts, pt, true, append([]byte(nil), []byte("conflicting-payload-"+fmt.Sprint(i))...))
				if err != nil {
					return nil, err
				}
				sc.Events = append(sc.Events, wireEvent{
					Raw: other, ArrivalMS: arrival + 5, LogicalIndex: i, SSRC: tr.AudioSSRC,
				})
			}
		}
	}

	for j := 0; j < tr.VideoCount; j++ {
		seq := uint16(60000 + j)
		raw, err := rtpio.MakeDatagram(tr.VideoSSRC, seq, uint32(j*1800), 97, j%2 == 0, []byte{0x80, byte(j), 0x01})
		if err != nil {
			return nil, err
		}
		arrival := int64(j*20 + 4 + tr.JitterMS[j%len(tr.JitterMS)]/3)
		sc.Events = append(sc.Events, wireEvent{Raw: raw, ArrivalMS: arrival, LogicalIndex: j, SSRC: tr.VideoSSRC})
		sc.VideoFrames = append(sc.VideoFrames, sync.Frame{
			Stream: "video", Index: j, PTS: uint64(j)*1800 + 240, DurationTicks: 1800,
		})
	}

	sort.SliceStable(sc.Events, func(i, j int) bool {
		return sc.Events[i].ArrivalMS < sc.Events[j].ArrivalMS
	})
	return sc, nil
}

func codecTicks(codec string) (uint64, error) {
	if codec == "opus" {
		d, err := media.ParseOpusTOC([]byte{0x08, 0x00})
		if err != nil {
			return 0, err
		}
		return d.ClockTicks, nil
	}
	d, err := media.AACFrameDuration(48_000, 1)
	if err != nil {
		return 0, err
	}
	return d.ClockTicks, nil
}

func audioPayload(codec string, index int) ([]byte, error) {
	if codec == "opus" {
		return []byte{0x08, byte(index + 1), 0xde, 0xad}, nil
	}
	au := []byte{0xaa, 0xbb, 0xcc, byte(index), 0x11, 0x22}
	auHeader := uint16(len(au) << 3)
	return []byte{0x00, 0x10, byte(auHeader >> 8), byte(auHeader), au[0], au[1], au[2], au[3], au[4], au[5]}, nil
}
