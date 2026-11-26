package pipeline

import (
	"fmt"
	"math/rand"

	"medcheck/ingest"
	"medcheck/internal/media"
	"medcheck/subtitle"
)

const (
	SegmentCount     = 12
	BlocksPerSegment = 8
	TotalPackets     = SegmentCount * BlocksPerSegment
	AUPerSegment     = 4
	CuesPerSegment   = 4
	SegmentMillis    = 4000
)

// Frame is an arrival-order packet plus a fixture-only CRC corruption marker.
type Frame struct {
	Packet     ingest.Packet
	CorruptCRC bool
}

// Fixture contains all co-generated samples for one demo case.
type Fixture struct {
	Case         string
	Seed         int64
	Source       ingest.Source
	Ordered      []ingest.Packet
	Frames       []Frame
	AUBytes      []byte
	CleanCues    []subtitle.Cue
	MalformedSRT string
}

// BuildFixture creates deterministic samples. The seed changes payload bytes,
// but not the required impairment counts.
func BuildFixture(caseName string, seed int64) *Fixture {
	rng := rand.New(rand.NewSource(seed))
	src := ingest.Source{Camera: "CAM-A", Stream: "video"}
	ordered := make([]ingest.Packet, 0, TotalPackets)
	for seq := uint64(0); seq < TotalPackets; seq++ {
		payload := make([]byte, 32)
		_, _ = rng.Read(payload)
		ordered = append(ordered, ingest.Packet{Header: ingest.Header{
			Camera: src.Camera, Stream: src.Stream, Seq: seq,
		}, Payload: payload})
	}

	f := &Fixture{Case: caseName, Seed: seed, Source: src, Ordered: ordered,
		CleanCues: buildCues(), AUBytes: buildAUBytes(rng)}
	for _, p := range ordered {
		f.Frames = append(f.Frames, Frame{Packet: p})
	}

	if caseName == "damaged" || caseName == "recover" {
		swapAdjacentPairs(f.Frames, []int{0, 4, 8, 12, 16, 20, 24, 28, 36})
		f.removeSequences(32, 33, 34, 35)
		duplicateSeqs := []uint64{2, 6, 10, 14, 18, 22}
		for i, seq := range duplicateSeqs {
			pkt := findPacket(ordered, seq)
			frame := Frame{Packet: pkt}
			// The recovery case still proves bad-CRC isolation without adding
			// an unrecoverable second loss to a parity row.
			if caseName == "recover" && i == 0 {
				frame.CorruptCRC = true
			}
			f.Frames = append(f.Frames, frame)
		}
		if caseName == "damaged" {
			for i := range f.Frames {
				if f.Frames[i].Packet.Seq == 39 {
					f.Frames[i].CorruptCRC = true
					break
				}
			}
		}
	}
	f.MalformedSRT = malformedSubtitleFixture()
	return f
}

func swapAdjacentPairs(frames []Frame, starts []int) {
	for _, s := range starts {
		if s+1 < len(frames) {
			frames[s], frames[s+1] = frames[s+1], frames[s]
		}
	}
}

func (f *Fixture) removeSequences(seqs ...uint64) {
	remove := map[uint64]bool{}
	for _, seq := range seqs {
		remove[seq] = true
	}
	out := f.Frames[:0]
	for _, frame := range f.Frames {
		if !remove[frame.Packet.Seq] {
			out = append(out, frame)
		}
	}
	f.Frames = out
}

func findPacket(packets []ingest.Packet, seq uint64) ingest.Packet {
	for _, p := range packets {
		if p.Seq == seq {
			return p
		}
	}
	return ingest.Packet{}
}

func buildCues() []subtitle.Cue {
	cues := make([]subtitle.Cue, 0, SegmentCount*CuesPerSegment)
	for seg := 0; seg < SegmentCount; seg++ {
		for j := 0; j < CuesPerSegment; j++ {
			start := int64(seg*SegmentMillis + j*1000)
			cues = append(cues, subtitle.Cue{
				StartMS: start,
				EndMS:   start + 1000,
				Text:    fmt.Sprintf("CAM-A 演出字幕 %02d-%d", seg+1, j+1),
			})
		}
	}
	return cues
}

func buildAUBytes(rng *rand.Rand) []byte {
	var blob []byte
	for seg := 0; seg < SegmentCount; seg++ {
		for j := 0; j < AUPerSegment; j++ {
			au := media.AccessUnit{
				Index:      uint32(seg*AUPerSegment + j),
				GOPID:      uint32(seg),
				PTSMS:      int64(seg*SegmentMillis + j*1000),
				IndexInGOP: uint16(j),
				KeyFrame:   j == 0,
				PayloadLen: 16,
			}
			blob = append(blob, media.EncodeHeader(au)...)
			payload := make([]byte, 16)
			_, _ = rng.Read(payload)
			blob = append(blob, payload...)
		}
	}
	return blob
}

func malformedSubtitleFixture() string {
	return `

bad source block without timing

900
00:00:45,000 --> 00:00:44,000
backwards timing

901
00:00:46,000 --> 00:00:47,000

902
not-a-timing-line
truncated block

903
00:00:48,000 --> 00:00:50,000
overlap first

904
00:00:49,000 --> 00:00:51,000
overlap second
`
}
