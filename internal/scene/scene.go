// Package scene assembles the acceptance scenario: a seeded MP4 clip with
// variable frame durations, an MXF clip with conflicting metadata, and a
// cross-node RTP stream with drift, wraps and packet faults.
//
// Everything here is deterministic in the seed, so two runs with identical
// arguments produce byte-identical artifacts.
package scene

import (
	"fmt"

	"relay.local/relay/internal/container"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/model"
)

// Params select fixture content. Zero values are replaced by fixture
// defaults.
type Params struct {
	Seed     int64
	Frames   int
	Width    int
	Height   int
	Segments int
}

// normalized fills defaults and clamps to the limits.
func (p Params) normalized() Params {
	if p.Frames <= 0 {
		p.Frames = fixture.DefaultFrames
	}
	if p.Width <= 0 {
		p.Width = fixture.DefaultWidth
	}
	if p.Height <= 0 {
		p.Height = fixture.DefaultHeight
	}
	if p.Segments <= 0 {
		p.Segments = fixture.DefaultSegments
	}
	return p
}

// Clip is one generated container with its essence and parsed form.
type Clip struct {
	Name   string
	Data   []byte
	File   *container.File
	Parsed bool
}

// Scenario is the full input set for one run.
type Scenario struct {
	Params Params
	MP4    Clip
	MXF    Clip
	RTP    fixture.RTPStream
	// Frames are the JPEG payloads of frames [0,Frames).
	Frames [][]byte
}

// Number of frames each clip carries. The MXF clip is offset so the two
// sources overlap on a shared region of the timeline.
const (
	// MXFFirst is the first global frame index carried by the MXF clip.
	MXFFirst = fixture.MXFFirstFrame
	// MXFCount is how many frames the MXF clip carries.
	MXFCount = 72
	// RTPFirst is the first global frame index carried by the RTP stream.
	RTPFirst = 12
)

// Build generates and parses every input.
func Build(p Params) (*Scenario, error) {
	p = p.normalized()
	s := &Scenario{Params: p}

	frames, err := fixture.FramesWithSeed(p.Seed, p.Frames, p.Width, p.Height)
	if err != nil {
		return nil, err
	}
	s.Frames = frames
	mxfFrames := frames[MXFFirst : MXFFirst+MXFCount]

	if s.MP4, err = buildMP4(p, frames); err != nil {
		return nil, err
	}
	if s.MXF, err = buildMXF(p, mxfFrames); err != nil {
		return nil, err
	}
	if s.RTP, err = buildRTP(p, frames, 0x51c0de+uint32(p.Seed&0xff)*0x100); err != nil {
		return nil, err
	}
	return s, nil
}

// buildMP4 writes and parses the MP4 clip. Its rotation comes from the
// container matrix while its colour comes from the sample entry, so the two
// description layers disagree and must be reconciled by precedence.
func buildMP4(p Params, frames [][]byte) (Clip, error) {
	spec := fixture.MP4Spec{MajorBrand: "isom"}
	spec.Tracks = append(spec.Tracks, fixture.MP4TrackSpec{
		ID: 1, Kind: model.Video, FourCC: "mjpg",
		Timescale: fixture.VideoTimescale,
		Width:     p.Width, Height: p.Height,
		Rotation:        90,
		ColrBox:         &fixture.ColrSpec{Primaries: 1, Transfer: 1, Matrix: 1, FullRange: false},
		ColrSampleEntry: &fixture.ColrSpec{Primaries: 9, Transfer: 16, Matrix: 9, FullRange: true},
		Samples:         mp4Samples(frames),
	})
	// A second, tiny audio-like data track proves multi-track handling and
	// that non-decodable codecs are reported rather than decoded.
	spec.Tracks = append(spec.Tracks, fixture.MP4TrackSpec{
		ID: 2, Kind: model.Audio, FourCC: "mp4a", Timescale: 48000,
		Samples: []fixture.SampleSpec{{Payload: []byte{0x11, 0x22, 0x33, 0x44}, Duration: 1024, Sync: true}},
	})
	data, err := fixture.BuildMP4(spec)
	if err != nil {
		return Clip{}, err
	}
	f, perr := container.ParseMP4("clip.mp4", data, container.DefaultLimits())
	if perr != nil {
		return Clip{}, fmt.Errorf("scene: generated mp4 failed to parse: %w", perr)
	}
	return Clip{Name: "clip.mp4", Data: data, File: f, Parsed: true}, nil
}

func mp4Samples(frames [][]byte) []fixture.SampleSpec {
	out := make([]fixture.SampleSpec, len(frames))
	for i := range frames {
		out[i] = fixture.SampleSpec{
			Payload:  frames[i],
			Duration: fixture.FrameDelta(i),
			Sync:     fixture.KeyFlag(i),
		}
	}
	return out
}

// buildMXF writes and parses the MXF clip whose container and bitstream layers
// disagree on rotation, crop and aperture.
func buildMXF(p Params, frames [][]byte) (Clip, error) {
	durs := make([]uint32, len(frames))
	keys := make([]bool, len(frames))
	for i := range frames {
		// Durations are in 90 kHz edit units and keep the VFR pattern.
		durs[i] = uint32(fixture.FrameDelta(MXFFirst+i) * 3)
		keys[i] = i%fixture.MXFKeyInterval == 0
	}
	spec := fixture.MXFSpec{Tracks: []fixture.MXFTrackSpec{{
		ID: 7, Kind: model.Video, Codec: model.CodecMJPEG,
		EditNum: fixture.MXFEditRate, EditDen: 1,
		Duration: uint64(len(frames)),
		Frames:   frames, Durations: durs, Keys: keys,
		Container: map[uint16]string{
			0x8007: "0",                 // rotation 0
			0x8009: "4,4,4,4",           // crop in container layer
			0x8015: fmt.Sprint(p.Width), // width
			0x8016: fmt.Sprint(p.Height),
		},
		Bitstream: map[uint16]string{
			0x8008: "90", // bitstream rotation 90 wins
			0x8019: fmt.Sprintf("%dx%d", p.Width-8, p.Height-8),
		},
	}}}
	data, err := fixture.BuildMXF(spec)
	if err != nil {
		return Clip{}, err
	}
	f, perr := container.ParseMXF("clip.mxf", data, container.DefaultLimits())
	if perr != nil {
		return Clip{}, fmt.Errorf("scene: generated mxf failed to parse: %w", perr)
	}
	return Clip{Name: "clip.mxf", Data: data, File: f, Parsed: true}, nil
}

// buildRTP generates the cross-node stream. The sender clock runs fast by
// DriftPPM and its timestamps wrap twice across the clip, which is what the
// wrap and drift stages must recover.
func buildRTP(p Params, frames [][]byte, ssrc uint32) (fixture.RTPStream, error) {
	first, count := RTPFirst, p.Frames-RTPFirst
	if count <= 0 {
		return fixture.RTPStream{}, fmt.Errorf("scene: %d frames cannot carry an RTP stream", p.Frames)
	}
	const driftPPM = fixture.DriftPPM
	var rf []fixture.RTPFrame
	// Start just below the 32-bit rollover so the stream crosses it twice:
	// once at the start and once after the full wrap.
	base := uint64(0xffffffff) - uint64(1<<32)/4
	for i := 0; i < count; i++ {
		global := first + i
		local := uint64(fixture.FrameTime90k(global))
		node := local + local*uint64(driftPPM)/1_000_000
		ts := uint32((base + node) & 0xffffffff)
		rf = append(rf, fixture.RTPFrame{TrackID: 7, Timestamp: ts, Payload: frames[global]})
	}
	faults := fixture.RTPFaults{
		Duplicates:    3,
		Reorders:      4,
		Drops:         2,
		RepeatPackets: 2,
	}
	return fixture.Packetize(rf, ssrc, faults, uint64(p.Seed)), nil
}

// Resolve applies metadata resolution to every parsed track in the scenario.
func (s *Scenario) Resolve(policy map[string]string) (*fault.List, []metadata.Conflict) {
	all := &fault.List{}
	var conflicts []metadata.Conflict
	for _, c := range []Clip{s.MP4, s.MXF} {
		if c.File == nil {
			continue
		}
		f, cf := metadata.ResolveAll(c.File.Tracks, metadata.Options{Policy: policy})
		for _, it := range f.Items {
			all.Add(it)
		}
		conflicts = append(conflicts, cf...)
	}
	return all, conflicts
}

// VideoTrack returns the first decodable video track of a clip.
func VideoTrack(c Clip) (*model.Track, bool) {
	if c.File == nil {
		return nil, false
	}
	for _, t := range c.File.Tracks {
		if t.Kind == model.Video {
			return t, true
		}
	}
	return nil, false
}
