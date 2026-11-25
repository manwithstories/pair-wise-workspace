// Package fixture generates every media sample used by tests and demos in code.
// No external fixture files are read.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/pairwise/mediad/internal/ingest"
	"github.com/pairwise/mediad/internal/repair"
)

// Scenario names accepted by the demo CLI.
const (
	ScenarioClean       = "clean"
	ScenarioDamaged     = "damaged"
	ScenarioMissingInit = "missing-init"
)

// Bundle contains generated manifests, tracks, bandwidth trace, and negative
// samples for one deterministic run.
type Bundle struct {
	Seed             int64
	Scenario         string
	Tracks           []repair.TrackInput
	Steps            []repair.BandwidthStep
	ConflictA        ingest.Packet
	ConflictB        ingest.Packet
	TruncatedBox     []byte
	OversizeBox      []byte
	MissingFieldHLS  []byte
	MissingFieldDASH []byte
}

var trackSpecs = []struct {
	id        string
	number    uint32
	timescale uint32
	baseSeq   uint64
	baseTS    uint32
}{
	{"track-01", 1, 90_000, 65_530, 0xfffff000},
	{"track-02", 2, 48_000, 65_530, 0xffffe000},
	{"track-03", 3, 44_100, 65_530, 0xffffd000},
}

const packetsPerSegment = 8
const segmentsPerTrack = 4

// Generate creates a deterministic scenario.
func Generate(seed int64, scenario string) *Bundle {
	rng := rand.New(rand.NewSource(seed))
	b := &Bundle{Seed: seed, Scenario: scenario}
	b.Steps = []repair.BandwidthStep{
		{AtGlobalSegment: 0, Bps: 5_000_000},
		{AtGlobalSegment: 2, Bps: 900_000},
		{AtGlobalSegment: 5, Bps: 2_400_000},
		{AtGlobalSegment: 8, Bps: 600_000},
		{AtGlobalSegment: 10, Bps: 3_500_000},
	}
	duplicateOffsets := map[int]bool{0: true, 2: true, 4: true, 6: true, 16: true, 18: true, 24: true, 26: true}
	lostOffsets := map[int]bool{10: true, 11: true, 12: true, 13: true}

	for ti, spec := range trackSpecs {
		track := repair.TrackInput{ID: spec.id, Number: spec.number, Timescale: spec.timescale}
		init, err := repair.BuildInitSegment(spec.id, spec.timescale)
		if err != nil {
			panic(err)
		}
		if scenario == ScenarioMissingInit && spec.id == "track-02" {
			init = nil
		}
		track.Init = init

		allPackets := make([]ingest.Packet, segmentsPerTrack*packetsPerSegment)
		for off := range allPackets {
			fullSeq := spec.baseSeq + uint64(off)
			segIdx := off / packetsPerSegment
			tfdt := uint64(segIdx) * unitsPerSegment(spec.timescale)
			rawSeq := uint16(fullSeq & 0xffff)
			rawTS := spec.baseTS + uint32(off*3600) // wraps near uint32
			payload := make([]byte, 48)
			rng.Read(payload)
			allPackets[off] = ingest.Packet{
				Sequence:  rawSeq,
				Timestamp: rawTS,
				SSRC:      0x100 + uint32(ti),
				Marker:    off%packetsPerSegment == packetsPerSegment-1,
				Payload:   payload,
			}
			_ = tfdt
		}

		for segIdx := 0; segIdx < segmentsPerTrack; segIdx++ {
			globalIndex := ti*segmentsPerTrack + segIdx
			startOffset := segIdx * packetsPerSegment
			endOffset := startOffset + packetsPerSegment - 1
			seg := repair.SegmentInput{
				TrackID:        spec.id,
				TrackNumber:    spec.number,
				GlobalIndex:    globalIndex,
				SegmentInTrack: segIdx,
				Timescale:      spec.timescale,
				DurationUnits:  unitsPerSegment(spec.timescale),
				TFDT:           uint64(segIdx) * unitsPerSegment(spec.timescale),
				Keyframe:       true,
				SeqStart:       spec.baseSeq + uint64(startOffset),
				SeqEnd:         spec.baseSeq + uint64(endOffset),
				BandwidthBps:   bandwidthAt(b.Steps, globalIndex),
			}
			// Source switches at global segments 4 and 8 are HLS discontinuities.
			seg.Discontinuity = globalIndex == 4 || globalIndex == 8
			// One damaged tfdt regression is repaired with evidence.
			if spec.id == "track-03" && segIdx == 3 {
				seg.TFDT = 123
			}
			seg.Payload = segmentPayload(seed, spec.id, segIdx, rng)

			anchor := allPackets[startOffset]
			var arrival []ingest.Packet
			arrival = append(arrival, anchor)
			var rest []ingest.Packet
			var retransmit []ingest.Packet
			for off := startOffset + 1; off <= endOffset; off++ {
				if ti == 0 && lostOffsets[off] {
					pkt := allPackets[off]
					retransmit = append(retransmit, pkt)
					continue
				}
				rest = append(rest, allPackets[off])
			}
			rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
			arrival = append(arrival, rest...)

			for off := startOffset; off <= endOffset; off++ {
				if duplicateOffsets[off] && !(ti == 0 && lostOffsets[off]) {
					arrival = append(arrival, allPackets[off])
				}
			}
			seg.Arrival = arrival
			// Missing-init scenario still includes retransmissions, but DAG blocks repair.
			seg.Retransmit = retransmit
			track.Segments = append(track.Segments, seg)
		}
		b.Tracks = append(b.Tracks, track)
	}

	b.ConflictA = ingest.Packet{Sequence: 4242, Timestamp: 42000, Payload: []byte("conflict-original")}
	b.ConflictB = ingest.Packet{Sequence: 4242, Timestamp: 42000, Payload: []byte("conflict-different")}
	good, _ := repair.BuildInitSegment("damaged", 90000)
	if len(good) > 12 {
		b.TruncatedBox = good[:len(good)-7]
	}
	oversize := make([]byte, 16)
	binary.BigEndian.PutUint32(oversize[0:4], uint32(repair.MaxFragmentBytes+1))
	copy(oversize[4:8], "big!")
	b.OversizeBox = oversize
	b.MissingFieldHLS = []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:2,\nx.m4s\n")
	b.MissingFieldDASH = []byte(`<MPD type="static"><Period><AdaptationSet><Representation id="x"/></AdaptationSet></Period></MPD>`)
	return b
}

func unitsPerSegment(timescale uint32) uint64 {
	return uint64(timescale) * 2 // two-second segments
}

func bandwidthAt(steps []repair.BandwidthStep, global int) int {
	bps := steps[0].Bps
	for _, step := range steps {
		if global >= step.AtGlobalSegment {
			bps = step.Bps
		}
	}
	return bps
}

func segmentPayload(seed int64, track string, seg int, rng *rand.Rand) []byte {
	prefix := []byte(fmt.Sprintf("mediad|seed=%d|track=%s|segment=%03d|", seed, track, seg))
	suffix := make([]byte, 64)
	_, _ = rng.Read(suffix)
	return append(prefix, suffix...)
}
