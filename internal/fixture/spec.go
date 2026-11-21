package fixture

import (
	"math/rand"

	"mediameld/internal/model"
)

// ColorSpec is a container-level colour description in the fixture.
type ColorSpec struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// CropSpec is a clean-aperture crop in pixels.
type CropSpec struct {
	X, Y, W, H int
}

// SampleSpec is one authored sample in a fixture track.
type SampleSpec struct {
	Data          []byte
	DTS, PTS, Dur int64
}

// TrackSpec describes a fixture track before it is serialized.
type TrackSpec struct {
	ID        uint32
	Kind      model.TrackKind
	Timescale int64
	Desc      string
	Width     int
	Height    int
	Rotation  int
	Color     *ColorSpec
	Crop      *CropSpec
	Samples   []SampleSpec
}

// Options controls fixture generation. All values are explicit so repeated
// runs with the same seed are byte-identical.
type Options struct {
	Seed     int64
	DriftPPM int64
	WrapAt   uint32
	VFR      []int64
	LateMS   int64
}

func defaultVFR(v []int64) []int64 {
	if len(v) > 0 {
		return v
	}
	return []int64{24, 25, 23}
}

// patternBytes produces deterministic pseudo-random sample bytes. The index
// and tag are embedded so distinct samples never collide by accident.
func patternBytes(r *rand.Rand, n int, tag byte, index int) []byte {
	b := make([]byte, n)
	r.Read(b)
	if n >= 8 {
		b[0], b[1], b[2] = tag, byte(index>>8), byte(index)
	}
	return b
}

// studioTracks builds the studio MP4: 24 fps video with a clean aperture and
// a 90-degree display rotation, plus a 48 kHz stereo audio track.
func studioTracks(seed int64) []TrackSpec {
	r := rand.New(rand.NewSource(seed))
	const tsVideo = 24000
	const delta = 1000
	v := TrackSpec{
		ID: 1, Kind: model.KindVideo, Timescale: tsVideo, Desc: "avc1",
		Width: 640, Height: 360, Rotation: 90,
		Color: &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
	}
	for i := 0; i < 48; i++ {
		v.Samples = append(v.Samples, SampleSpec{
			Data: patternBytes(r, 8000+i%37, 'V', i),
			DTS:  int64(i) * delta,
			PTS:  int64(i)*delta + 2*delta,
			Dur:  delta,
		})
	}
	const tsAudio = 48000
	const adelta = 4000
	a := TrackSpec{ID: 2, Kind: model.KindAudio, Timescale: tsAudio, Desc: "mp4a"}
	for i := 0; i < 24; i++ {
		a.Samples = append(a.Samples, SampleSpec{
			Data: patternBytes(r, 900+i%11, 'A', i),
			DTS:  int64(i) * adelta,
			PTS:  int64(i) * adelta,
			Dur:  adelta,
		})
	}
	return []TrackSpec{v, a}
}

// cadenceTicks returns exact per-frame durations in the given timescale for
// the VFR cycle, using the same integer cadence the timeline engine uses.
func cadenceTicks(rates []int64, n int, timescale int64) []int64 {
	rs := make([]model.Rational, len(rates))
	for i, r := range rates {
		rs[i] = model.Rational{Num: r, Den: 1}
	}
	c, err := model.NewCadence(rs)
	if err != nil {
		panic(err)
	}
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		out[i] = c.Start(int64(i), timescale)
	}
	return out
}

// CameraVideoSamples is the number of VFR frames the camera delivers.
const CameraVideoSamples = 72

// cameraTracks builds the MXF camera: a VFR video track and two audio tracks.
// The third track is the boom channel used to exercise tail isolation.
func cameraTracks(seed int64, rates []int64) []TrackSpec {
	r := rand.New(rand.NewSource(seed + 7919))
	const tsVideo = 90000
	starts := cadenceTicks(rates, CameraVideoSamples, tsVideo)
	v := TrackSpec{
		ID: 3, Kind: model.KindVideo, Timescale: tsVideo, Desc: "mxf-vc3",
		Width: 640, Height: 360, Rotation: 0,
		Crop:  &CropSpec{X: 8, Y: 8, W: 632, H: 352},
		Color: &ColorSpec{Primaries: 1, Transfer: 1, Matrix: 1},
	}
	for i := 0; i < CameraVideoSamples; i++ {
		end := int64(0)
		if i+1 < CameraVideoSamples {
			end = starts[i+1]
		} else {
			end = starts[i] + (starts[i] - starts[i-1])
		}
		v.Samples = append(v.Samples, SampleSpec{
			Data: patternBytes(r, 9000+i%53, 'C', i),
			DTS:  starts[i], PTS: starts[i], Dur: end - starts[i],
		})
	}
	const tsAudio = 48000
	const adelta = 4000
	mkAudio := func(id uint32, tag byte, n int) TrackSpec {
		t := TrackSpec{ID: id, Kind: model.KindAudio, Timescale: tsAudio, Desc: "mxf-pcm"}
		for i := 0; i < n; i++ {
			t.Samples = append(t.Samples, SampleSpec{
				Data: patternBytes(r, 1200+i%13, tag, i),
				DTS:  int64(i) * adelta, PTS: int64(i) * adelta, Dur: adelta,
			})
		}
		return t
	}
	return []TrackSpec{v, mkAudio(4, 'B', 24), mkAudio(5, 'D', 24)}
}

// rtpSchedule describes the transport timestamps for the VFR camera track.
type rtpSchedule struct {
	Timestamps []uint32 // 32-bit RTP timestamps including wrap
	TruePTS    []int64  // exact source ticks (90 kHz)
	Indexes    []int
}

func buildRTP(seed int64, rates []int64, driftPPM int64, wrapAt uint32) rtpSchedule {
	starts := cadenceTicks(rates, 48, 90000)
	s := rtpSchedule{}
	for i, t := range starts {
		fast := t * (1_000_000 + driftPPM) / 1_000_000
		s.Timestamps = append(s.Timestamps, uint32((uint64(wrapAt)+uint64(fast))&0xFFFFFFFF))
		s.TruePTS = append(s.TruePTS, t)
		s.Indexes = append(s.Indexes, i)
	}
	return s
}
