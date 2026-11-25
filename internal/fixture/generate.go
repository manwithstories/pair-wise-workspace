// Package fixture writes deterministic Matroska/WebM samples and the RTP
// streams that carry them, so every test and demo runs without external media
// or network access. All output is derived from an explicit seed.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"avcheck/internal/ebml"
)

// TrackSpec describes one track to synthesize.
type TrackSpec struct {
	Number  uint64
	UID     uint64
	Kind    string // "video", "audio" or "cue"
	Codec   string
	Name    string
	Frames  int
	StartNs int64
	StepNs  int64
	Payload int
	Depends []uint64
}

// Spec is a whole sample definition.
type Spec struct {
	Seed          int64
	TimecodeScale int64
	DurationNs    int64
	Tracks        []TrackSpec
	// Orphan, when set, adds a block for an undeclared track.
	Orphan *OrphanBlock
}

// DefaultSpec returns the three-track climbing-coach sample: coach audio,
// climbing video and timestamped action cues. Durations are round numbers so
// the report thresholds have a stable target.
func DefaultSpec(seed int64) Spec {
	return Spec{
		Seed:          seed,
		TimecodeScale: 1_000_000,
		DurationNs:    8_000_000_000,
		Tracks: []TrackSpec{
			// Video leads the audio clock by a small, deliberate offset and
			// the cue track by a slightly larger one, so the demo reports real
			// (but in-tolerance) cross-track skew instead of an artificial 0.
			{Number: 1, UID: 0x101, Kind: "video", Codec: "V_VP8", Name: "climb", Frames: 80, StartNs: 10_000_000, StepNs: 100_000_000, Payload: 3000},
			{Number: 2, UID: 0x102, Kind: "audio", Codec: "A_OPUS", Name: "coach", Frames: 400, StartNs: 0, StepNs: 20_000_000, Payload: 1600},
			{Number: 3, UID: 0x103, Kind: "cue", Codec: "AVCHECK_CUE", Name: "hints", Frames: 40, StartNs: 25_000_000, StepNs: 200_000_000, Payload: 500, Depends: []uint64{1}},
		},
	}
}

// Block describes one generated frame on the wire.
type Block struct {
	Track   uint64
	Ticks   int64
	Payload []byte
	Key     bool
}

// Sample is the generated sample: the container bytes plus the ground-truth
// block schedule used by tests and by the transport simulation.
type Sample struct {
	Spec   Spec
	Bytes  []byte
	Blocks []Block
	// FaultOffset is the byte offset a corruption targeted (0 when clean).
	FaultOffset int64
}

// BuildWithExtraBlock muxes an additional block for a track that the spec does
// not declare, producing the "orphan block" fault fixture.
func BuildWithExtraBlock(spec Spec, track uint64, ticks int64, payload []byte) (*Sample, error) {
	spec.Orphan = &OrphanBlock{Track: track, Ticks: ticks, Payload: payload}
	return Build(spec)
}

// OrphanBlock is a block deliberately authored for an undeclared track.
type OrphanBlock struct {
	Track   uint64
	Ticks   int64
	Payload []byte
}

// Build renders the spec as a Matroska file. Payload bytes are a stable
// function of (seed, track, frame) so identical seeds yield identical files.
func Build(spec Spec) (*Sample, error) {
	if len(spec.Tracks) == 0 {
		return nil, fmt.Errorf("fixture: empty track list")
	}
	w := newWriter()
	blocks := make([]Block, 0, 512)
	rng := rand.New(rand.NewSource(spec.Seed))

	w.master(ebml.IDEBML, func(w *writer) {
		w.uint(0x4286, 1) // EBMLVersion
		w.uint(0x42F7, 1) // EBMLReadVersion
		w.uint(0x42F2, 4) // EBMLMaxIDLength
		w.uint(0x42F3, 8) // EBMLMaxSizeLength
		w.str(ebml.IDDocType, "webm")
		w.uint(0x4287, 4) // DocTypeVersion
		w.uint(0x4285, 2) // DocTypeReadVersion
	})

	seg := w.openMaster(ebml.IDSegment)
	w.master(ebml.IDInfo, func(w *writer) {
		w.uint(0x2AD7B1, uint64(spec.TimecodeScale)) // TimecodeScale
		w.str(0x4D80, "avcheck")                     // MuxingApp
		w.str(0x5741, "avcheck")                     // WritingApp
		w.float(0x4489, float64(spec.DurationNs)/float64(spec.TimecodeScale))
	})
	w.master(ebml.IDTracks, func(w *writer) {
		for _, t := range spec.Tracks {
			w.master(0xAE, func(w *writer) {
				w.uint(0xD7, t.Number)
				w.uint(0x73C5, t.UID)
				w.uint(0x83, trackTypeCode(t.Kind))
				w.str(0x86, t.Codec)
				w.str(0x536E, t.Name)
				if t.StepNs > 0 {
					w.uint(0x23E383, uint64(t.StepNs))
				}
				for _, d := range t.Depends {
					w.uint(0x4EC7, d)
				}
			})
		}
	})

	// Interleave tracks by timestamp in one cluster per second, exercising
	// out-of-order-free but genuinely interleaved blocks.
	var all []sched
	for _, t := range spec.Tracks {
		for i := 0; i < t.Frames; i++ {
			ns := t.StartNs + int64(i)*t.StepNs
			ticks := ns / spec.TimecodeScale
			data := payloadFor(rng, t, i)
			all = append(all, sched{track: t.Number, ticks: ticks, key: t.Kind == "video", data: data})
		}
	}
	if spec.Orphan != nil {
		all = append(all, sched{track: spec.Orphan.Track, ticks: spec.Orphan.Ticks,
			data: spec.Orphan.Payload})
	}
	sortSched(all)

	clusterNs := int64(1_000_000_000)
	var cur *writer
	var clusterStart int64 = -1
	flush := func() {
		if cur != nil {
			cur.closeMaster(seg)
			cur = nil
		}
	}
	for _, s := range all {
		ns := s.ticks * spec.TimecodeScale
		if clusterStart < 0 || ns-clusterStart >= clusterNs {
			flush()
			clusterStart = ns - ns%clusterNs
			cur = w.openMaster(ebml.IDCluster)
			w.uint(ebml.IDTimecode, uint64(clusterStart/spec.TimecodeScale))
		}
		rel := int16((ns - clusterStart) / spec.TimecodeScale)
		w.simpleBlock(s.track, rel, s.key, s.data)
		blocks = append(blocks, Block{Track: s.track, Ticks: s.ticks, Payload: s.data, Key: s.key})
	}
	flush()
	w.closeMaster(seg)
	return &Sample{Spec: spec, Bytes: w.buf.Bytes(), Blocks: blocks}, nil
}

// sched is one scheduled frame before it is muxed.
type sched struct {
	track uint64
	ticks int64
	key   bool
	data  []byte
}

func sortSched(all []sched) {
	// Insertion sort keeps it allocation-free and stable by (ticks, track).
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			a, b := all[j-1], all[j]
			if a.ticks < b.ticks || (a.ticks == b.ticks && a.track <= b.track) {
				break
			}
			all[j-1], all[j] = b, a
		}
	}
}

func trackTypeCode(kind string) uint64 {
	switch kind {
	case "video":
		return 1
	case "audio":
		return 2
	case "cue":
		return 17
	}
	return 0
}

// payloadFor generates deterministic per-(track,frame) bytes. Cue payloads
// are readable hint text so a misaligned cue is obvious in diagnostics.
func payloadFor(rng *rand.Rand, t TrackSpec, frame int) []byte {
	if t.Kind == "cue" {
		s := fmt.Sprintf("hint-%02d:%04d", frame, rng.Intn(1000))
		b := []byte(s)
		for len(b) < t.Payload {
			b = append(b, '.')
		}
		return b[:t.Payload]
	}
	b := make([]byte, t.Payload)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	if len(b) >= 2 {
		binary.BigEndian.PutUint16(b, uint16(t.Number))
	}
	return b
}
