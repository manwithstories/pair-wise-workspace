package fixture

import (
	"math/rand"
)

// Sample names reported in the demo summary.
const (
	SampleStudio       = "studio-interview"
	SampleLocation     = "location-b-roll"
	SampleRemote       = "remote-narration"
	SampleTruncated    = "corrupt-truncated"
	SampleBadBlock     = "corrupt-out-of-range"
	SampleAudioClash   = "corrupt-audio-clash"
	SampleRecovery     = "recovery-interrupted"
	SampleColourClash  = "corrupt-colour-clash"
)

// rng is the deterministic payload source shared by all templates.
func newRNG(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// StudioInterview is the reference three-track sample:棚内访谈 video +
// host audio, interleaved with a second camera angle and a keyframe every
// ~200 ms.
func StudioInterview(seed int64) Document {
	rng := newRNG(seed)
	tracks := []TrackSpec{
		{Number: 1, UID: 101, Type: TrackTypeVideo, Codec: CodecVP9,
			DefaultDurNS: 33_333_333, Matrix: 9, Transfer: 16, Primar: 9},
		{Number: 2, UID: 102, Type: TrackTypeVideo, Codec: CodecVP8,
			DefaultDurNS: 40_000_000, Matrix: 1, Transfer: 1, Primar: 1},
		{Number: 3, UID: 103, Type: TrackTypeAudio, Codec: CodecOpus,
			Channels: 2, SampleRate: 48000, BitDepth: 16, DefaultDurNS: 20_000_000},
	}
	var blocks []BlockSpec
	for ms := int64(0); ms < 1000; ms += 40 {
		track := uint8(1)
		if ms%80 == 40 {
			track = 2
		}
		blocks = append(blocks, BlockSpec{
			Track: track, TimeMS: ms,
			Keyframe: ms%200 == 0, Payload: randPayload(rng, 24),
		})
	}
	for ms := int64(0); ms < 1000; ms += 20 {
		blocks = append(blocks, BlockSpec{
			Track: 3, TimeMS: ms, Payload: randPayload(rng, 16),
		})
	}
	SortBlocks(blocks)
	return Document{
		Seed: seed, TimecodeScale: 1_000_000, DurationMS: 1000,
		Tracks: tracks, Blocks: blocks, UnknownSegment: true,
	}
}

// LocationBRoll is an exterior-camera sample with a mono field-audio track
// at a different sample rate; the merge layer must keep both audio clocks.
func LocationBRoll(seed int64) Document {
	rng := newRNG(seed + 7)
	tracks := []TrackSpec{
		{Number: 1, UID: 201, Type: TrackTypeVideo, Codec: CodecVP9,
			DefaultDurNS: 41_708_333, Matrix: 9, Transfer: 18, Primar: 9},
		{Number: 2, UID: 202, Type: TrackTypeAudio, Codec: CodecPCM,
			Channels: 1, SampleRate: 44100, BitDepth: 16, DefaultDurNS: 22_675_737},
	}
	var blocks []BlockSpec
	for ms := int64(0); ms < 800; ms += 42 {
		blocks = append(blocks, BlockSpec{Track: 1, TimeMS: ms,
			Keyframe: ms%210 == 0, Payload: randPayload(rng, 20)})
	}
	for ms := int64(0); ms < 800; ms += 23 {
		blocks = append(blocks, BlockSpec{Track: 2, TimeMS: ms,
			Payload: randPayload(rng, 8)})
	}
	SortBlocks(blocks)
	return Document{Seed: seed, TimecodeScale: 1_000_000, DurationMS: 800,
		Tracks: tracks, Blocks: blocks}
}

// RemoteNarration is the off-site commentary: one audio track with an
// unknown channel layout (Channels omitted), forcing an explicit mapping
// decision before the timeline can consume it.
func RemoteNarration(seed int64) Document {
	rng := newRNG(seed + 13)
	tracks := []TrackSpec{
		{Number: 1, UID: 301, Type: TrackTypeAudio, Codec: CodecOpus,
			Channels: 0, SampleRate: 24000, BitDepth: 16}, // layout unknown
		{Number: 2, UID: 302, Type: TrackTypeVideo, Codec: CodecVP8,
			DefaultDurNS: 100_000_000},
	}
	var blocks []BlockSpec
	for ms := int64(0); ms < 600; ms += 20 {
		blocks = append(blocks, BlockSpec{Track: 1, TimeMS: ms,
			Payload: randPayload(rng, 12)})
	}
	for ms := int64(0); ms < 600; ms += 100 {
		blocks = append(blocks, BlockSpec{Track: 2, TimeMS: ms,
			Keyframe: true, Payload: randPayload(rng, 18)})
	}
	SortBlocks(blocks)
	return Document{Seed: seed, TimecodeScale: 1_000_000, DurationMS: 600,
		Tracks: tracks, Blocks: blocks, UnknownCluster: true}
}

// ColourClash builds two video tracks whose colour metadata disagree; the
// merge layer must record the conflict by fixed source priority instead
// of inventing a value.
func ColourClash(seed int64) Document {
	doc := StudioInterview(seed)
	doc.Seed = seed
	for i := range doc.Tracks {
		if doc.Tracks[i].Type == TrackTypeVideo {
			switch doc.Tracks[i].Number {
			case 1:
				doc.Tracks[i].Matrix, doc.Tracks[i].Transfer, doc.Tracks[i].Primar = 9, 16, 9
			case 2:
				doc.Tracks[i].Matrix, doc.Tracks[i].Transfer, doc.Tracks[i].Primar = 1, 1, 1
			}
		}
	}
	return doc
}

// AudioClash places two audio tracks with incompatible sample rates and no
// mapping rule inside one document; the normalizer must block downstream.
func AudioClash(seed int64) Document {
	rng := newRNG(seed + 29)
	tracks := []TrackSpec{
		{Number: 1, UID: 401, Type: TrackTypeAudio, Codec: CodecOpus,
			Channels: 2, SampleRate: 48000, BitDepth: 16},
		{Number: 2, UID: 402, Type: TrackTypeAudio, Codec: CodecPCM,
			Channels: 6, SampleRate: 22050, BitDepth: 16},
	}
	var blocks []BlockSpec
	for ms := int64(0); ms < 400; ms += 20 {
		blocks = append(blocks,
			BlockSpec{Track: 1, TimeMS: ms, Payload: randPayload(rng, 12)},
			BlockSpec{Track: 2, TimeMS: ms + 5, Payload: randPayload(rng, 12)},
		)
	}
	SortBlocks(blocks)
	return Document{Seed: seed, TimecodeScale: 1_000_000, DurationMS: 400,
		Tracks: tracks, Blocks: blocks}
}

// SortBlocks orders blocks by time, then track number, so the timeline
// receives a stable interleaved stream even within one cluster.
func SortBlocks(blocks []BlockSpec) {
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0; j-- {
			a, b := blocks[j-1], blocks[j]
			if a.TimeMS < b.TimeMS || (a.TimeMS == b.TimeMS && a.Track <= b.Track) {
				break
			}
			blocks[j-1], blocks[j] = b, a
		}
	}
}
