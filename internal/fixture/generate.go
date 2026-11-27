// Package fixture generates deterministic AVPK sample streams entirely in
// memory: no external files, network or codecs are involved. A seed drives
// every payload and every fault injection point, so the same parameters
// always reproduce byte-identical fixtures.
package fixture

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"

	"avflow/internal/model"
	"avflow/internal/stream"
)

// Track identifiers used by every generated timeline.
const (
	TrackVideo      uint32 = 1
	TrackAudio      uint32 = 2
	TrackTransition uint32 = 3
)

// FaultSet selects which defects the generator injects. The empty set yields
// a fully clean stream; AllFaults injects every supported defect exactly once.
type FaultSet struct {
	Truncated       bool
	BadCRC          bool
	PTSRegression   bool
	MissingKeyframe bool
	ChannelConflict bool
	RateConflict    bool
	ColorConflict   bool
}

// AllFaults enables every supported fault class.
func AllFaults() FaultSet {
	return FaultSet{true, true, true, true, true, true, true}
}

// RecoverableFaults enables every fault class the pipeline can crop or
// normalize while still producing a committed timeline.
func RecoverableFaults() FaultSet {
	return FaultSet{
		Truncated: true, BadCRC: true, MissingKeyframe: true,
		ChannelConflict: true, RateConflict: true, ColorConflict: true,
	}
}

// None returns an empty (clean) fault set.
func None() FaultSet { return FaultSet{} }

// Plan describes one deterministic fixture.
type Plan struct {
	Seed    int64
	Packets int // total packets spread across the three tracks
	Faults  FaultSet
}

// Generated is one rendered fixture plus the precise locations of every
// injected defect, used by tests to assert localization accuracy.
type Generated struct {
	Tracks      []stream.TrackDesc
	Packets     []stream.PacketSpec
	Data        []byte
	FaultSeqs   map[model.FaultType]uint64
	FirstFault  model.FaultType
}

// StandardTracks builds the canonical three-track directory.
func StandardTracks(audioRate uint32, layout, channels uint8) []stream.TrackDesc {
	return []stream.TrackDesc{
		{ID: TrackVideo, Kind: stream.KindVideo, TimeBase: model.Rational{Num: 1, Den: 90000},
			ColorTag: stream.ColorSRGB, Width: 160, Height: 120,
			MaxKeyIntervalNS: 500_000_000, Name: "host-video"},
		{ID: TrackAudio, Kind: stream.KindAudio, TimeBase: model.Rational{Num: 1, Den: int64(audioRate)},
			Channels: channels, ChannelLayout: layout, SampleRate: audioRate, Name: "player-voice"},
		{ID: TrackTransition, Kind: stream.KindTransition, TimeBase: model.Rational{Num: 1, Den: 90000},
			ColorTag: stream.ColorBT709, Name: "story-transition"},
	}
}

// Build renders the plan into an encoded AVPK byte stream.
func Build(p Plan) (*Generated, error) {
	rng := rand.New(rand.NewSource(p.Seed))
	// The rate-conflict fault switches the declared audio rate to 44.1k. The
	// packet PTS and frame counts are generated against that same time base
	// from the start, so a rate-conflict stream is internally consistent but
	// disagrees with the canonical 48k timeline the pipeline normalizes to.
	audioRate := uint32(48000)
	if p.Faults.RateConflict {
		audioRate = 44100
	}
	framesPerPacket := int(audioRate / 100) // 10 ms of samples
	tracks := StandardTracks(audioRate, stream.LayoutStereo, 2)
	g := &Generated{FaultSeqs: map[model.FaultType]uint64{}}
	perTrack := p.Packets / 3
	specs := make([]stream.PacketSpec, 0, p.Packets)
	seq := uint64(0)
	for i := 0; i < perTrack; i++ {
		pts := int64(i * 3000)
		key := i%6 == 0
		tag := stream.ColorSRGB
		specs = append(specs, stream.PacketSpec{
			Track: TrackVideo, Seq: seq, PTS: pts, Duration: 3000, Keyframe: key,
			Payload: videoPayload(rng, i, tag),
		})
		seq++
	}
	// Audio: stereo, one packet holds 10 ms of sample frames on the track's
	// own sample-rate time base.
	audioStart := seq
	for i := 0; i < perTrack; i++ {
		specs = append(specs, stream.PacketSpec{
			Track: TrackAudio, Seq: seq, PTS: int64(i * framesPerPacket),
			Duration: uint32(framesPerPacket),
			Payload:  audioPayload(rng, 2, framesPerPacket),
		})
		seq++
	}
	// Transition clips.
	for i := 0; i < p.Packets-2*perTrack; i++ {
		specs = append(specs, stream.PacketSpec{
			Track: TrackTransition, Seq: seq, PTS: int64(i * 9000), Duration: 4500,
			Keyframe: true, Payload: transitionPayload(rng),
		})
		seq++
	}
	g.injectFaults(p, tracks, specs, audioStart, perTrack)
	g.Tracks = tracks
	g.Packets = specs
	var buf bytes.Buffer
	if err := stream.Encode(&buf, tracks, specs); err != nil {
		return nil, err
	}
	g.Data = buf.Bytes()
	return g, nil
}

func (g *Generated) injectFaults(p Plan, tracks []stream.TrackDesc, specs []stream.PacketSpec, audioStart uint64, perTrack int) {
	vid := func(i int) *stream.PacketSpec { return &specs[i] }
	aud := func(i int) *stream.PacketSpec { return &specs[perTrack+i] }
	mark := func(ft model.FaultType, s uint64) {
		if _, ok := g.FaultSeqs[ft]; !ok {
			g.FaultSeqs[ft] = s
		}
	}
	// 1) Bad CRC on a mid video packet -> must propagate to index and crop.
	if p.Faults.BadCRC && perTrack > 8 {
		v := vid(perTrack / 2)
		v.CorruptCRC = true
		mark(model.FaultBadCRC, v.Seq)
	}
	// 2) PTS regression on the audio track -> non-recoverable, triggers rollback.
	if p.Faults.PTSRegression && perTrack > 4 {
		a := aud(2)
		prev := aud(1).PTS
		a.PTS = prev - 100
		mark(model.FaultPTSRegression, a.Seq)
	}
	// 3) Missing keyframe: remove keyframe flags across a long video span.
	if p.Faults.MissingKeyframe && perTrack > 14 {
		for i := 7; i <= 12; i++ {
			vid(i).Keyframe = false
		}
		mark(model.FaultMissingKeyframe, vid(13).Seq)
	}
	// 4) Channel conflict: one audio packet with a ragged payload size.
	if p.Faults.ChannelConflict && perTrack > 2 {
		a := aud(perTrack-1)
		a.Payload = a.Payload[:len(a.Payload)-1]
		mark(model.FaultChannelConflict, a.Seq)
	}
	// 5) Sample-rate conflict: the descriptor already uses 44.1k (chosen in
	// Build); record the first audio packet as the localization point.
	if p.Faults.RateConflict {
		mark(model.FaultRateConflict, audioStart)
	}
	// 6) Color tag conflict: one video packet labeled BT.709 on an sRGB track.
	if p.Faults.ColorConflict && perTrack > 5 {
		v := vid(3)
		v.ColorTag = stream.ColorBT709
		mark(model.FaultColorConflict, v.Seq)
	}
	// 7) Truncated tail is applied by the caller at byte level (Truncate()).
	if p.Faults.Truncated {
		last := specs[len(specs)-1].Seq
		mark(model.FaultTruncated, last)
	}
	// First fault on the global timeline = smallest injected packet order.
	g.FirstFault = model.FaultType("")
	bestOrder := -1
	for i, s := range specs {
		ft := g.faultTypeOf(s, p.Faults)
		if ft == "" {
			continue
		}
		if bestOrder == -1 || i < bestOrder {
			bestOrder, g.FirstFault = i, ft
		}
	}
}

func (g *Generated) faultTypeOf(s stream.PacketSpec, f FaultSet) model.FaultType {
	switch {
	case s.CorruptCRC:
		return model.FaultBadCRC
	case f.ChannelConflict && s.Track == TrackAudio && len(s.Payload)%2 != 0:
		return model.FaultChannelConflict
	case f.ColorConflict && s.ColorTag == stream.ColorBT709:
		return model.FaultColorConflict
	}
	return ""
}

// Truncate returns the encoded bytes with the final packet's tail removed,
// producing the tail-truncation defect at the packet boundary.
func (g *Generated) Truncate() []byte {
	if len(g.Data) <= 4 {
		return g.Data
	}
	return g.Data[:len(g.Data)-3]
}

func videoPayload(rng *rand.Rand, i int, tag uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{uint8(x*16 + i), uint8(y*20), uint8(rng.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func audioPayload(rng *rand.Rand, channels, frames int) []byte {
	out := make([]byte, channels*frames*2)
	for i := 0; i < channels*frames; i++ {
		v := int16(rng.Intn(4000) - 2000)
		out[i*2] = byte(uint16(v))
		out[i*2+1] = byte(uint16(v) >> 8)
	}
	return out
}

func transitionPayload(rng *rand.Rand) []byte {
	b := make([]byte, 16)
	rng.Read(b)
	return b
}
