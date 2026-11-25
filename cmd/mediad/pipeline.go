package main

import (
	"math/rand"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/fingerprint"
	"mediad/internal/mp4"
	medirtp "mediad/internal/rtp"
	"mediad/internal/silence"
	"mediad/internal/timeline"
)

// segTicks is the uniform segment grid: 100 ms on the 48 kHz base.
const segTicks = 4800

// pipeOut collects everything the verifier asserts on.
type pipeOut struct {
	hashes     []string
	silent     []silence.Interval
	dedupMulti int
	dups       int
	conflicts  int
	gaps       int
	unrecov    int
	samples    []mp4.Sample
}

// genStreams builds the three ordered streams. The remote track echoes
// studio packets 30..33 (segment 5) so cross-track dedup has content to
// find. Each track uses its own sub-seed for reproducibility.
func genStreams(seed int64) map[string][]*rtp.Packet {
	specs := defaultTracks()
	studio := genTrack(specs[0], rand.New(rand.NewSource(seed)))
	echo := map[int][]byte{}
	for i := 30; i <= 33; i++ {
		echo[i] = studio[i].Payload
	}
	specs[2].echo = echo
	return map[string][]*rtp.Packet{
		"studio": studio,
		"field":  genTrack(specs[1], rand.New(rand.NewSource(seed+1))),
		"remote": genTrack(specs[2], rand.New(rand.NewSource(seed+2))),
	}
}

// normalFaults are the recoverable weak-network faults every run sees.
func normalFaults(id string) faultSpec {
	switch id {
	case "studio":
		return faultSpec{
			duplicateSeqs: []uint16{3, 17},
			reorderAt:     []int{5, 21, 33},
			conflictSeq:   -1,
			lateSeq:       -1,
			bandwidth:     []float64{1e6, 1e6, 250e3, 1e6},
		}
	case "field":
		return faultSpec{duplicateSeqs: []uint16{7}, reorderAt: []int{10},
			conflictSeq: -1, lateSeq: -1}
	default:
		return faultSpec{duplicateSeqs: []uint16{40}, reorderAt: []int{12},
			conflictSeq: -1, lateSeq: -1}
	}
}

// damageFaults adds unrecoverable damage to the studio track: a 5-packet
// burst loss (segment 6), a payload conflict (quarantined, segment 1) and
// a packet arriving after the wait window closed (segment 8).
func damageFaults(f faultSpec) faultSpec {
	f.burstStart, f.burstCount = 34, 5 // ts 28800..33600 @48k
	f.conflictSeq = 6                  // ts 5280 @48k
	f.lateSeq = 50                     // ts 42240 @48k
	return f
}

func bwAt(f faultSpec, pushIdx int) float64 {
	if len(f.bandwidth) == 0 {
		return 1e6
	}
	i := pushIdx / 15
	if i >= len(f.bandwidth) {
		i = len(f.bandwidth) - 1
	}
	return f.bandwidth[i]
}

// runPipeline executes reorder -> parse -> segmentize -> fingerprint /
// silence / dedup over the three tracks. With damage, the studio track
// additionally suffers burst loss, a conflict and a late packet.
func runPipeline(seed int64, damage bool) (*pipeOut, error) {
	streams := genStreams(seed)
	out := &pipeOut{}
	var tracks []timeline.Track
	for _, spec := range defaultTracks() {
		f := normalFaults(spec.id)
		if damage && spec.id == "studio" {
			f = damageFaults(f)
		}
		arrival := injectFaults(streams[spec.id], f)
		re := medirtp.New(2, 8)
		var ordered []*rtp.Packet
		for i, p := range arrival {
			re.AdjustForBandwidth(bwAt(f, i), 1e6)
			o, err := re.Push(p)
			ordered = append(ordered, o...)
			if err != nil {
				// Conflict packets are quarantined; the stream continues.
				continue
			}
		}
		ordered = append(ordered, re.Flush()...)
		st := re.Stats()
		out.dups += st.Duplicates
		out.conflicts += len(st.Conflicts)
		out.gaps += len(medirtp.MergeGaps(st.Gaps))
		for _, g := range st.Gaps {
			if !g.Recoverable {
				out.unrecov++
			}
		}
		parsed, err := codec.Parse(ordered, spec.c, spec.base)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, timeline.Track{ID: spec.id, Units: parsed.Units})
		header := 0 // AAC: payload is the raw frame
		if spec.c == codec.Opus {
			header = 1 // Opus: skip the TOC byte
		}
		out.silent = append(out.silent, silence.Detect(spec.id, parsed.Units, 0.02, header)...)
		if spec.id == "studio" {
			for _, u := range parsed.Units {
				if u.Dur.Ticks > 0 { // skip zero-duration keep-alives
					out.samples = append(out.samples, mp4.Sample{
						Data:     u.Payload,
						Duration: uint32(u.Dur.Convert(clock.Base48k).Ticks),
					})
				}
			}
		}
	}
	segs := timeline.Segmentize(tracks, clock.At(segTicks, clock.Base48k), clock.Base48k)
	for _, s := range segs {
		out.hashes = append(out.hashes, fingerprint.HashSegment(s))
	}
	for _, srcs := range fingerprint.Dedup(segs) {
		if len(srcs) > 1 {
			out.dedupMulti++
		}
	}
	return out, nil
}
