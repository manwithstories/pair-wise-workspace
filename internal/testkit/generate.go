// Package testkit generates deterministic RTP/RTCP capture streams:
// clean, damaged (burst loss, reorder, duplicates, timestamp faults,
// truncation) and wraparound boundary variants.
package testkit

import (
	"encoding/json"
	"io"
	"math/rand"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"mediaroute/internal/packet"
)

const (
	VideoSSRC = 4369 // 0x1111
	AudioSSRC = 8738 // 0x2222

	videoRate = 90000
	audioRate = 48000
	videoStep = 3000  // 30 fps
	audioStep = 960   // 20 ms
	srEvery   = 100   // packets between Sender Reports per track
	ntpBase   = 3_900_000_000
)

// Options controls stream generation.
type Options struct {
	Seed            int64
	PacketsPerTrack int
	GOP             int
	VideoSeqBase    uint16
	AudioSeqBase    uint16
	VideoTSBase     uint32
	AudioTSBase     uint32
	NoDamage        bool
}

// DefaultOptions returns the demo configuration.
func DefaultOptions(seed int64) Options {
	return Options{Seed: seed, PacketsPerTrack: 6000, GOP: 60}
}

// DamageEvent records one injected fault (ground truth).
type DamageEvent struct {
	Kind  string `json:"kind"`
	Track string `json:"track"`
	Seq   uint64 `json:"seq"`
	Note  string `json:"note"`
}

// Result summarizes a generated stream.
type Result struct {
	Packets         int           `json:"packets"`
	Tracks          int           `json:"tracks"`
	DamagedFirstSeq uint64        `json:"damaged_first_seq"`
	Damage          []DamageEvent `json:"damage"`
}

// Deterministic damage offsets (from the video/audio sequence base).
const (
	dmgLossStart  = 4812
	dmgLossCount  = 30
	dmgTruncNAL   = 5200
	dmgBackwardTS = 5500
	dmgReorderA   = 6004
	dmgReorderB   = 6010
	dmgLate       = 6400
	dmgLateAfter  = 6950
	dmgConflict   = 7000
	dmgAudioDup   = 5000
)

func videoPayload(r *rand.Rand, keyframe bool) []byte {
	n := 200 + r.Intn(1000)
	b := make([]byte, n)
	if keyframe {
		b[0] = 0x65 // IDR
	} else {
		b[0] = 0x41
	}
	for i := 1; i < n; i++ {
		b[i] = byte(r.Intn(256))
	}
	return b
}

func audioPayload(r *rand.Rand) []byte {
	n := 100 + r.Intn(100)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

func ntpFor(i int, step, rate uint32) uint64 {
	nanos := int64(ntpBase)*1_000_000_000 + int64(i)*int64(step)*1_000_000_000/int64(rate)
	sec := uint64(nanos) / 1_000_000_000
	frac := (uint64(nanos) % 1_000_000_000) * (1 << 32) / 1_000_000_000
	return sec<<32 | frac
}

func writeSR(w io.Writer, ssrc, rtpTS uint32, ntp uint64) error {
	sr := &rtcp.SenderReport{SSRC: ssrc, NTPTime: ntp, RTPTime: rtpTS}
	raw, err := sr.Marshal()
	if err != nil {
		return err
	}
	return packet.WriteRecord(w, packet.KindRTCP, raw)
}

func writeRTP(w io.Writer, ssrc uint32, pt uint8, seq uint16, ts uint32, payload []byte) error {
	p := &rtp.Packet{
		Header: rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq,
			Timestamp: ts, SSRC: ssrc, Marker: payload[0] == 0x65},
		Payload: payload,
	}
	raw, err := p.Marshal()
	if err != nil {
		return err
	}
	return packet.WriteRecord(w, packet.KindRTP, raw)
}

// Generate writes the stream and returns ground-truth metadata.
func Generate(w io.Writer, opts Options) (*Result, error) {
	r := rand.New(rand.NewSource(opts.Seed))
	sess := packet.Session{Tracks: []packet.TrackDesc{
		{SSRC: VideoSSRC, ClockRate: videoRate, Kind: "video"},
		{SSRC: AudioSSRC, ClockRate: audioRate, Kind: "audio", DependsOn: []uint32{VideoSSRC}},
	}}
	rawSess, err := json.Marshal(sess)
	if err != nil {
		return nil, err
	}
	if err := packet.WriteRecord(w, packet.KindSession, rawSess); err != nil {
		return nil, err
	}

	n := opts.PacketsPerTrack
	// Pre-build payloads so duplicates can reuse identical bytes.
	vPay := make([][]byte, n)
	aPay := make([][]byte, n)
	for i := 0; i < n; i++ {
		vPay[i] = videoPayload(r, i%opts.GOP == 0)
		aPay[i] = audioPayload(r)
	}
	vTS := func(i int) uint32 {
		ts := opts.VideoTSBase + uint32(i)*videoStep
		if !opts.NoDamage && i >= dmgBackwardTS {
			ts -= 180000 // permanent 2 s backward jump
		}
		return ts
	}
	vSeq := func(i int) uint16 { return opts.VideoSeqBase + uint16(i) }
	aSeq := func(i int) uint16 { return opts.AudioSeqBase + uint16(i) }

	emitVideo := func(i int) error {
		pay := vPay[i]
		if !opts.NoDamage && i == dmgTruncNAL {
			pay = pay[:5] // truncated NAL unit
		}
		return writeRTP(w, VideoSSRC, 96, vSeq(i), vTS(i), pay)
	}

	res := &Result{Packets: 2 * n, Tracks: 2}
	if !opts.NoDamage {
		res.DamagedFirstSeq = uint64(opts.VideoSeqBase) + dmgLossStart
		res.Damage = []DamageEvent{
			{Kind: "burst-loss", Track: "video", Seq: res.DamagedFirstSeq, Note: "30 packets dropped"},
			{Kind: "truncated-nal", Track: "video", Seq: uint64(opts.VideoSeqBase) + dmgTruncNAL},
			{Kind: "backward-ts", Track: "video", Seq: uint64(opts.VideoSeqBase) + dmgBackwardTS},
			{Kind: "reorder", Track: "video", Seq: uint64(opts.VideoSeqBase) + dmgReorderA},
			{Kind: "late", Track: "video", Seq: uint64(opts.VideoSeqBase) + dmgLate},
			{Kind: "conflict-dup", Track: "video", Seq: uint64(opts.VideoSeqBase) + dmgConflict},
			{Kind: "exact-dup", Track: "audio", Seq: uint64(opts.AudioSeqBase) + dmgAudioDup},
		}
	}

	lost := func(i int) bool {
		return !opts.NoDamage && i >= dmgLossStart && i < dmgLossStart+dmgLossCount
	}
	// swapMap implements the two reorder swaps.
	swap := map[int]int{dmgReorderA: dmgReorderA + 1, dmgReorderA + 1: dmgReorderA,
		dmgReorderB: dmgReorderB + 1, dmgReorderB + 1: dmgReorderB}

	for i := 0; i < n; i++ {
		// Sender Reports lead the packets they anchor so clocks are
		// never estimated at track start.
		if i%srEvery == 0 {
			if err := writeSR(w, VideoSSRC, vTS(i), ntpFor(i, videoStep, videoRate)); err != nil {
				return nil, err
			}
			if err := writeSR(w, AudioSSRC, opts.AudioTSBase+uint32(i)*audioStep, ntpFor(i, audioStep, audioRate)); err != nil {
				return nil, err
			}
		}
		j := i
		if !opts.NoDamage {
			if s, ok := swap[i]; ok {
				j = s
			}
		}
		if !lost(j) && (opts.NoDamage || j != dmgLate) {
			if err := emitVideo(j); err != nil {
				return nil, err
			}
		}
		if !opts.NoDamage && i == dmgLateAfter {
			if err := emitVideo(dmgLate); err != nil { // arrives too late
				return nil, err
			}
		}
		if !opts.NoDamage && i == dmgConflict {
			bad := make([]byte, len(vPay[i]))
			for k := range bad {
				bad[k] = 0xEE
			}
			if err := writeRTP(w, VideoSSRC, 96, vSeq(i), vTS(i), bad); err != nil {
				return nil, err
			}
		}
		if err := writeRTP(w, AudioSSRC, 97, aSeq(i), opts.AudioTSBase+uint32(i)*audioStep, aPay[i]); err != nil {
			return nil, err
		}
		if !opts.NoDamage && i == dmgAudioDup {
			if err := writeRTP(w, AudioSSRC, 97, aSeq(i), opts.AudioTSBase+uint32(i)*audioStep, aPay[i]); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}
