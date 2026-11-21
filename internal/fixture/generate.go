package fixture

import (
	"math/rand"
	"sort"

	mrtp "mediacheck/internal/rtp"

	"github.com/pion/rtp"
)

// Fragment duration shared by both tracks: 0.5 s.
const (
	fragDurVideoUnits = VideoTimescale / 2 // 45000
	fragDurAudioUnits = AudioTimescale / 2 // 24000
	audioFragFrames   = AudioPayloadRate / 2
	videoPayloadSize  = 4096
	// audioOffsetUnits shifts the audio track by 15 ms so the segment
	// cutter must round and av_sync has a deterministic nonzero value.
	audioOffsetUnits = AudioTimescale * 15 / 1000 // 720
)

// Options controls deterministic fixture generation.
type Options struct {
	Seed      int64
	Fragments int // fragments per track
	LossBurst int // consecutive packets dropped in one burst
	Bandwidth []float64
	Case      string // "normal" | "corrupt"
}

// Stream is the generated input: RTP arrivals plus expectations used
// by the pipeline and the report.
type Stream struct {
	Arrivals      []mrtp.Arrival
	Case          string
	FragCount     int // valid fragments per track
	TotalLost     int // packets removed by the loss model
	CorruptFrags  int // corrupt fragments injected (case=corrupt)
	AudioSilentAt [2]int
}

// frag is one generated fragment before packetization.
type frag struct {
	track   string // "audio" | "video"
	trackID uint32
	seq     uint32
	data    []byte
}

// Generate builds the deterministic fragment set and pushes it through
// the simulated network (loss, duplicates, reorder, retransmission).
func Generate(opt Options) *Stream {
	if opt.Fragments <= 0 {
		opt.Fragments = 8
	}
	if len(opt.Bandwidth) == 0 {
		opt.Bandwidth = []float64{3}
	}
	frags := buildFragments(opt)
	st := &Stream{Case: opt.Case, FragCount: opt.Fragments}
	st.Arrivals, st.TotalLost = packetize(frags, opt, 1000, 0)
	if opt.Case == "corrupt" {
		corrupt := corruptFragments(opt)
		st.CorruptFrags = len(corrupt)
		// Corrupt fragments arrive after the valid stream, continuing
		// the same RTP sequence space (one session), without loss:
		// every damaged case must deterministically reach the parser.
		var last float64
		for _, a := range st.Arrivals {
			if a.ArrivalMs > last {
				last = a.ArrivalMs
			}
		}
		for i, f := range corrupt {
			st.Arrivals = append(st.Arrivals, mrtp.Arrival{
				Packet: &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						SequenceNumber: uint16(1000 + len(frags) + i),
						Timestamp:      uint32(900000 + i*4000),
						SSRC:           0xC0FFEE,
					},
					Payload: f.data,
				},
				ArrivalMs:     last + 50 + float64(i)*10,
				BandwidthMbps: opt.Bandwidth[len(opt.Bandwidth)-1],
			})
		}
	}
	return st
}

// buildFragments creates the valid fragment set for both tracks,
// including the planted mfhd gap and tfdt rollback.
func buildFragments(opt Options) []frag {
	var frags []frag
	phase := 0.0
	for i := 0; i < opt.Fragments; i++ {
		// Video: seq 1..N. Fragments 2,3 duplicate the payloads of
		// fragments 0,1 so the cutter produces one duplicate segment.
		vseq := uint32(i + 1)
		dupOf := -1
		switch i {
		case 2:
			dupOf = 0
		case 3:
			dupOf = 1
		}
		vp := videoPayload(i, videoPayloadSize, dupOf)
		frags = append(frags, frag{
			track: "video", trackID: VideoTrackID, seq: vseq,
			data: buildFragment(fragParams{
				trackID: VideoTrackID, seq: vseq,
				tfdt: uint64(i) * fragDurVideoUnits, tfdtVersion: 1,
				sampleDur: 3000, sampleCount: 15, payload: vp,
			}),
		})

		// Audio: planted mfhd gap — seq 6 is never generated.
		aseq := uint32(i + 1)
		if i >= 5 {
			aseq++ // skip seq 6
		}
		tfdt := uint64(i)*fragDurAudioUnits + audioOffsetUnits
		if i == 4 {
			// Planted tfdt rollback: 100 ms before the expected base.
			tfdt -= AudioTimescale / 10
		}
		silent := [2]int{-1, -1}
		fullSilence := false
		switch i {
		case 2:
			silent = [2]int{audioFragFrames / 4, audioFragFrames * 3 / 4}
		case 5, 6:
			fullSilence = true // two identical silent fragments
		}
		pcm, endPhase := audioPCM(audioFragFrames, phase, silent, fullSilence)
		phase = endPhase
		frags = append(frags, frag{
			track: "audio", trackID: AudioTrackID, seq: aseq,
			data: buildFragment(fragParams{
				trackID: AudioTrackID, seq: aseq,
				tfdt: tfdt, tfdtVersion: 1,
				sampleDur: AudioTimescale / 10, sampleCount: 5, payload: pcm,
			}),
		})
	}
	// Interleave by media time so the RTP stream mixes tracks.
	sort.SliceStable(frags, func(i, j int) bool {
		return frags[i].seq < frags[j].seq
	})
	return frags
}

// packetize wraps fragments in RTP packets and applies the network
// model: burst + isolated loss, duplicates, reorder, retransmission.
// seqBase is the first RTP sequence number; timeBase shifts all
// arrival times. It returns arrivals sorted by time and the loss count.
func packetize(frags []frag, opt Options, seqBase uint16, timeBase float64) ([]mrtp.Arrival, int) {
	rng := rand.New(rand.NewSource(opt.Seed))
	bw := opt.Bandwidth
	if len(bw) == 0 {
		bw = []float64{3}
	}
	type pkt struct {
		wire *rtp.Packet
		size int
		lost bool
	}
	var pkts []pkt
	for i, f := range frags {
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: seqBase + uint16(i),
				Timestamp:      uint32(2000 + i*4000),
				SSRC:           0xC0FFEE,
			},
			Payload: f.data,
		}
		pkts = append(pkts, pkt{wire: p, size: len(f.data) + 12})
	}

	// Loss model: one burst plus isolated losses.
	n := len(pkts)
	burst := opt.LossBurst
	if burst > n/2 {
		burst = n / 2
	}
	start := 0
	if n > burst+1 {
		start = rng.Intn(n - burst)
	}
	for i := start; i < start+burst; i++ {
		pkts[i].lost = true
	}
	for i := range pkts {
		if !pkts[i].lost && rng.Float64() < 0.02 {
			pkts[i].lost = true
		}
	}
	totalLost := 0
	for _, p := range pkts {
		if p.lost {
			totalLost++
		}
	}

	// Arrival times: serialization delay under the bandwidth profile
	// plus small deterministic jitter.
	var arrivals []mrtp.Arrival
	now := timeBase
	bwAt := func(t float64, total float64) float64 {
		if total <= 0 {
			return bw[0]
		}
		seg := total / float64(len(bw))
		idx := int(t / seg)
		if idx >= len(bw) {
			idx = len(bw) - 1
		}
		return bw[idx]
	}
	// First pass: estimate total duration for the profile slices.
	totalBits := 0
	for _, p := range pkts {
		totalBits += p.size * 8
	}
	avgBw := 0.0
	for _, b := range bw {
		avgBw += b
	}
	avgBw /= float64(len(bw))
	totalMs := float64(totalBits) / (avgBw * 1000) // bits / (mbit/s) = ms

	lostIdx := 0
	for i, p := range pkts {
		curBw := bwAt(now, totalMs)
		now += float64(p.size*8) / (curBw * 1000)
		now += rng.Float64() * 3 // jitter 0..3 ms
		if p.lost {
			// Bounded recovery: every 10th lost packet is
			// unrecoverable; the rest are retransmitted. With large
			// bursts one retransmission is deliberately too late and
			// gets isolated by the reorder window.
			lostIdx++
			if lostIdx%10 == 0 {
				continue // never retransmitted
			}
			delay := 30 + rng.Float64()*90 // 30..120 ms
			if lostIdx == 3 && opt.LossBurst >= 10 {
				delay = 400 // late beyond any wait limit
			}
			arrivals = append(arrivals, mrtp.Arrival{
				Packet: p.wire, ArrivalMs: now + delay, BandwidthMbps: curBw,
			})
			continue
		}
		arrivals = append(arrivals, mrtp.Arrival{
			Packet: p.wire, ArrivalMs: now, BandwidthMbps: curBw,
		})
		// Duplicates (deterministic): counted by the reorderer,
		// never re-emitted.
		if i%4 == 1 {
			arrivals = append(arrivals, mrtp.Arrival{
				Packet: p.wire, ArrivalMs: now + 5, BandwidthMbps: curBw,
			})
		}
	}
	// Reorder: swap a few adjacent arrival times.
	for k := 0; k+1 < len(arrivals); k += 7 {
		arrivals[k], arrivals[k+1] = arrivals[k+1], arrivals[k]
	}
	sort.SliceStable(arrivals, func(i, j int) bool {
		return arrivals[i].ArrivalMs < arrivals[j].ArrivalMs
	})
	return arrivals, totalLost
}
