package testkit

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"archive.local/recover/internal/clock"
	"archive.local/recover/internal/index"
	"archive.local/recover/internal/media"
)

// Case names understood by Generate.
const (
	CaseSuccess      = "success"
	CaseCorruptIndex = "corrupt-index"
	CaseWrapVFRDrift = "wrap-vfr-drift"
)

// Options configures a generated fixture.
type Options struct {
	Seed       int64
	DurationNs int64
	Bitrates   []int
	Loss       float64
	Case       string
}

// DefaultOptions returns the acceptance-demo parameters.
func DefaultOptions() Options {
	return Options{Seed: 17, DurationNs: 120_000_000_000, Bitrates: []int{800, 1600, 3200}, Loss: 0.03, Case: CaseSuccess}
}

// Rendition is one generated bitrate version and all of its input artifacts.
type Rendition struct {
	Name           string
	Bitrate        int
	Master         []byte  // ISO BMFF archive master
	Index          []byte  // sidecar keyframe index (offsets relative to mdat payload)
	Clock          []int64 // capture-clock ticks per video sample
	AudioClock     []int64 // capture-clock ticks per audio sample
	Anchors        []clock.Anchor
	AudioAnchors   []clock.Anchor
	ProgramNs      []int64 // ground-truth video program time
	AudioProgramNs []int64 // ground-truth audio program time
	VideoTimescale uint32
	AudioTimescale uint32
	Codec          string
	DurationNs     int64
}

// Fixture is a complete, self-generated demonstration input set.
type Fixture struct {
	Options    Options
	Renditions []Rendition
	RTP        *RTPFixture
	Notes      []string
}

const (
	videoTimescale = 90000
	audioTimescale = 48000
	frameTicks     = 3600 // 25 fps at 90 kHz
	audioFrameTick = 1024
)

// Generate builds a deterministic fixture for the requested case.
func Generate(opts Options) (*Fixture, error) {
	if opts.DurationNs <= 0 || opts.DurationNs > 6*3600*1e9 {
		return nil, fmt.Errorf("duration %d ns out of range", opts.DurationNs)
	}
	if len(opts.Bitrates) == 0 || len(opts.Bitrates) > 8 {
		return nil, fmt.Errorf("bitrate count %d out of range", len(opts.Bitrates))
	}
	if opts.Case == "" {
		opts.Case = CaseSuccess
	}
	switch opts.Case {
	case CaseSuccess, CaseCorruptIndex, CaseWrapVFRDrift:
	default:
		return nil, fmt.Errorf("unknown case %q", opts.Case)
	}
	bitrates := append([]int(nil), opts.Bitrates...)
	sort.Ints(bitrates)
	f := &Fixture{Options: opts}
	for i, br := range bitrates {
		r, err := generateRendition(opts, i, br)
		if err != nil {
			return nil, err
		}
		f.Renditions = append(f.Renditions, *r)
	}
	// Packet cadence is 10 ms, so the reorder-buffer latency stays well under
	// the 500 ms acceptance bound while still being measurable.
	packets := int(opts.DurationNs / 10_000_000)
	if packets < 1 {
		packets = 1
	}
	rtp, err := GenerateRTP(opts.Seed, opts.Loss, packets)
	if err != nil {
		return nil, err
	}
	f.RTP = rtp
	return f, nil
}

func generateRendition(opts Options, idx, bitrate int) (*Rendition, error) {
	vfr := opts.Case == CaseWrapVFRDrift
	frames := int(opts.DurationNs / (frameTicks * 1e9 / videoTimescale))
	if frames < 4 {
		frames = 4
	}
	durations := make([]uint32, frames)
	program := make([]int64, frames)
	for i := 0; i < frames; i++ {
		d := uint32(frameTicks)
		if vfr {
			// Every 12th pair stretches then compresses, keeping 25 fps on
			// average while exercising VFR handling.
			switch i % 12 {
			case 10:
				d = 5400
			case 11:
				d = 1800
			}
		}
		durations[i] = d
		if i > 0 {
			program[i] = program[i-1] + int64(d)*1e9/videoTimescale
		}
	}
	lastFrameEnd := program[frames-1] + int64(durations[frames-1])*1e9/videoTimescale

	// Annex B stream: one NAL per access unit, IDR (5) on keyframes.
	keyframeStride := 100 // 4 s
	var stream []byte
	sizes := make([]uint32, frames)
	sync := make([]bool, frames)
	keySample := make([]int, 0, frames/keyframeStride+1)
	for i := 0; i < frames; i++ {
		isKey := i%keyframeStride == 0
		sync[i] = isKey
		nalType := byte(0x41)
		if isKey {
			nalType = 0x65
			keySample = append(keySample, i)
		}
		start := len(stream)
		payloadLen := 12 + (i*13)%40
		stream = append(stream, 0, 0, 0, 1, nalType)
		for j := 0; j < payloadLen; j++ {
			stream = append(stream, byte((i*31+j*7)&0xff))
		}
		sizes[i] = uint32(len(stream) - start)
	}

	// Audio: 48 kHz AAC-like frames.
	audioFrames := int(lastFrameEnd * int64(audioTimescale) / (audioFrameTick * 1e9))
	if audioFrames < 1 {
		audioFrames = 1
	}
	audioProgram := make([]int64, audioFrames)
	audioSizes := make([]uint32, audioFrames)
	audioSync := make([]bool, audioFrames)
	for i := 0; i < audioFrames; i++ {
		if i > 0 {
			audioProgram[i] = audioProgram[i-1] + int64(audioFrameTick)*1e9/audioTimescale
		}
		audioSizes[i] = 48
		audioSync[i] = true
	}
	audioPayload := make([]byte, audioFrames*48)

	videoSamples := make([]media.SampleSpec, frames)
	for i := range durations {
		videoSamples[i] = media.SampleSpec{Duration: durations[i], Size: sizes[i], Sync: sync[i]}
	}
	audioSamples := make([]media.SampleSpec, audioFrames)
	for i := range audioSamples {
		audioSamples[i] = media.SampleSpec{Duration: audioFrameTick, Size: 48, Sync: true}
	}
	master, err := media.BuildMP4([]media.TrackSpec{
		{ID: 1, Kind: "video", Timescale: videoTimescale, Codec: "h264", Width: 1920, Height: 1080, Payload: stream, Samples: videoSamples},
		{ID: 2, Kind: "audio", Timescale: audioTimescale, Codec: "aac", Channels: 2, Payload: audioPayload, Samples: audioSamples},
	})
	if err != nil {
		return nil, fmt.Errorf("rendition %d: build master: %w", bitrate, err)
	}

	// Capture clocks: distinct drift per rendition, wrap forced mid-capture.
	driftPPM := []int64{600, -400, 900, 250, -750, 500, 1200, -150}[idx%8]
	rawClock := captureClock(program, videoTimescale, driftPPM, true)
	audioDrift := -driftPPM / 2
	rawAudio := captureClock(audioProgram, audioTimescale, audioDrift, false)

	if opts.Case == CaseWrapVFRDrift {
		// A real tape restart: a small backward step and a duplicated timestamp,
		// then a +0.5 s clock reset at 60% that anchors must segment around.
		if frames > 2000 {
			rawClock[1500] = rawClock[1499] - 1800
			rawClock[750] = rawClock[749]
		}
		breakAt := frames * 6 / 10
		const jump = int64(500 * videoTimescale / 1000)
		for i := breakAt; i < len(rawClock); i++ {
			rawClock[i] = (rawClock[i] + jump) & 0xffffffff
		}
		if audioFrames > 10 {
			rawAudio[audioFrames/2] = rawAudio[audioFrames/2-1]
		}
	}

	anchors := makeAnchors(program, opts.Seed+int64(idx)*7, 10)
	audioAnchors := makeAnchors(audioProgram, opts.Seed+int64(idx)*13, 10)

	// Index pages, one per keyframe, ordinal-addressed like a page table.
	var pages []index.Entry
	for ord, s := range keySample {
		pages = append(pages, index.Entry{
			Sample: ord,
			Offset: int64(nalsOffset(stream, sizes, s)),
			Size:   int64(sizes[s]),
			PTS:    rawClock[s],
		})
	}
	if opts.Case == CaseCorruptIndex && len(pages) >= 4 {
		// One entry points past the end of the bitstream and one page is
		// missing; the index still covers the tail, so repair must reconcile.
		bad := len(pages) / 2
		pages[bad].Offset = int64(len(stream)) + 4096
		drop := bad - 1
		pages = append(pages[:drop], pages[drop+1:]...)
	}
	idxBytes := index.Encode(videoTimescale, pages)

	return &Rendition{
		Name:           fmt.Sprintf("%dk", bitrate),
		Bitrate:        bitrate,
		Master:         master,
		Index:          idxBytes,
		Clock:          rawClock,
		AudioClock:     rawAudio,
		Anchors:        anchors,
		AudioAnchors:   audioAnchors,
		ProgramNs:      program,
		AudioProgramNs: audioProgram,
		VideoTimescale: videoTimescale,
		AudioTimescale: audioTimescale,
		Codec:          "h264",
		DurationNs:     lastFrameEnd,
	}, nil
}

// nalsOffset returns the byte offset of sample s in the concatenated stream.
func nalsOffset(stream []byte, sizes []uint32, s int) int {
	var off uint32
	for i := 0; i < s; i++ {
		off += sizes[i]
	}
	if int(off) >= len(stream) {
		return len(stream) - 1
	}
	return int(off)
}

// captureClock converts program nanoseconds into wrapped capture ticks with a
// fixed rate error. With forceWrap, the counter starts near its 32-bit ceiling
// so a full rollover happens about 60% into the capture.
func captureClock(program []int64, timescale uint32, driftPPM int64, forceWrap bool) []int64 {
	out := make([]int64, len(program))
	if len(program) == 0 {
		return out
	}
	span := program[len(program)-1]
	totalTicks := span * int64(timescale) / 1e9
	if totalTicks < 0 {
		totalTicks = 0
	}
	var base int64
	if forceWrap {
		base = (int64(1) << 32) - totalTicks*6/10
	} else {
		base = 12345
	}
	for i, ns := range program {
		ticks := ns * int64(timescale) / 1e9
		v := base + ticks + ticks*driftPPM/1e6
		out[i] = v & 0xffffffff
	}
	return out
}

// makeAnchors samples trusted program-time references every intervalNs with a
// small deterministic measurement jitter.
func makeAnchors(program []int64, seed int64, intervalNs int64) []clock.Anchor {
	if len(program) == 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(seed))
	step := intervalNs * 1e9
	var out []clock.Anchor
	var next int64
	for i, p := range program {
		if p < next && i != 0 {
			continue
		}
		if i == 0 || p >= next {
			jitter := int64((rng.Float64()*2 - 1) * 400_000) // +/- 0.4 ms
			out = append(out, clock.Anchor{Sample: i, ProgramNs: p + jitter, Source: "external-sync"})
			next = p + step
		}
	}
	return out
}

// Inputs returns the ordered input artifacts as raw bytes, keyed by the name
// used in reports. The resume journal digests exactly this set, so a change in
// any rendered artifact is detected.
func (f *Fixture) Inputs() map[string][]byte {
	out := make(map[string][]byte, 2*len(f.Renditions))
	for i := range f.Renditions {
		r := &f.Renditions[i]
		out[r.Name+".master.mp4"] = r.Master
		out[r.Name+".keyidx"] = r.Index
	}
	return out
}

// P95 returns the 95th percentile of a sample set, using nearest-rank.
func P95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	rank := int(math.Ceil(0.95*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}
