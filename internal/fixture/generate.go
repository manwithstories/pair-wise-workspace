// Package fixture generates self-contained CMAF/fMP4 test streams: valid,
// corrupt and boundary-case fragments, plus RTP packets carrying a wrapping
// 33-bit capture clock as the drift reference.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"

	"github.com/pion/rtp"
)

const (
	VideoTrackID   = 1
	AudioTrackID   = 2
	VideoTimescale = 90000
	AudioTimescale = 48000

	videoFragDur  = 90000 // 1s of video per fragment
	audioFragDur  = 48000 // 1s of audio per fragment
	framesPerFrag = 30
	corruptAt     = 4 // video fragment index the corrupt modes hit
)

// CorruptMode selects the fault injected into the generated stream.
type CorruptMode string

const (
	CorruptNone           CorruptMode = ""
	CorruptTruncatedBox   CorruptMode = "truncated-box"
	CorruptSampleRange    CorruptMode = "sample-range"
	CorruptSeqGap         CorruptMode = "sequence-gap"
	CorruptTFDTRegression CorruptMode = "tfdt-regression"
	CorruptDuplicate      CorruptMode = "duplicate-segment"
)

// ParseCorruptMode validates a mode name.
func ParseCorruptMode(s string) (CorruptMode, error) {
	switch m := CorruptMode(s); m {
	case CorruptNone, CorruptTruncatedBox, CorruptSampleRange,
		CorruptSeqGap, CorruptTFDTRegression, CorruptDuplicate:
		return m, nil
	}
	return "", fmt.Errorf("unknown corrupt mode %q (valid: truncated-box, sample-range, sequence-gap, tfdt-regression, duplicate-segment)", s)
}

// Options controls stream generation.
type Options struct {
	Seed      int64
	Fragments int // video fragments; the same number of audio fragments
	DriftPPM  float64
	WrapAt    uint64 // capture-clock modulus; 0 disables wrap
	VFR       bool
	Corrupt   CorruptMode
}

func (o *Options) defaults() {
	if o.Fragments <= 0 {
		o.Fragments = 12
	}
}

// Stream is a generated input: init segment plus interleaved fragments.
type Stream struct {
	Init      []byte
	Fragments [][]byte // video fragment i at index 2i, audio at 2i+1
	Capture   [][]byte // marshalled RTP packet per video fragment; nil for audio
}

// Generate builds a stream. Generation is fully deterministic for a seed.
func Generate(opts Options) *Stream {
	opts.defaults()
	rng := rand.New(rand.NewSource(opts.Seed))
	n := opts.Fragments
	s := &Stream{Init: buildInit([]trackSpec{
		{VideoTrackID, VideoTimescale, 3000, 0},
		{AudioTrackID, AudioTimescale, 1024, 0},
	})}

	drift := 1 + opts.DriftPPM/1e6
	vTfdt := make([]uint64, n+1)
	for i := 1; i <= n; i++ {
		vTfdt[i] = uint64(math.Round(float64(i) * videoFragDur * drift))
	}

	videoSamples := make([][]sampleSpec, n)
	for i := 0; i < n; i++ {
		videoSamples[i] = genVideoFragment(rng, opts, vTfdt[i+1]-vTfdt[i])
	}
	if opts.Corrupt == CorruptDuplicate && n >= corruptAt+4 {
		// Repeat the content of fragments 2,3 at 6,7 with fresh seq/tfdt.
		videoSamples[corruptAt+2] = cloneSamples(videoSamples[2])
		videoSamples[corruptAt+3] = cloneSamples(videoSamples[3])
	}

	for i := 0; i < n; i++ {
		if opts.Corrupt == CorruptSeqGap && i == corruptAt {
			continue // drop one video fragment: mfhd gap, no invented samples
		}
		tfdt := vTfdt[i]
		if opts.Corrupt == CorruptTFDTRegression && i == corruptAt+1 {
			tfdt = vTfdt[i-1] // rewind tfdt into the previous fragment's range
		}
		if opts.Corrupt == CorruptSampleRange && i == corruptAt {
			videoSamples[i][len(videoSamples[i])-1].size += 4096 // exceeds mdat
		}
		frag := buildFragment(uint32(i+1), VideoTrackID, tfdt, videoSamples[i])
		if opts.Corrupt == CorruptTruncatedBox && i == corruptAt {
			frag = frag[:len(frag)*3/5]
		}
		s.Fragments = append(s.Fragments, frag)
		s.Capture = append(s.Capture, capturePacket(opts, i))
		aTfdt := uint64(i) * audioFragDur
		s.Fragments = append(s.Fragments, buildFragment(uint32(i+1), AudioTrackID, aTfdt, genAudioFragment(i)))
		s.Capture = append(s.Capture, nil)
	}
	return s
}

// framePattern returns 30 frame durations summing to one second.
func framePattern(vfr bool) []uint32 {
	d := make([]uint32, framesPerFrag)
	for i := range d {
		if vfr && i%2 == 0 {
			d[i] = 2700
		} else if vfr {
			d[i] = 3300
		} else {
			d[i] = 3000
		}
	}
	return d
}

func genVideoFragment(rng *rand.Rand, opts Options, fragDelta uint64) []sampleSpec {
	base := framePattern(opts.VFR)
	drift := 1 + opts.DriftPPM/1e6
	out := make([]sampleSpec, framesPerFrag)
	var sum uint64
	for j := 0; j < framesPerFrag; j++ {
		dur := uint64(math.Round(float64(base[j]) * drift))
		size := uint32(300 + rng.Intn(400))
		flags := uint32(0x10000) // non-sync
		if j == 0 {
			size = uint32(1200 + rng.Intn(600))
			flags = 0x2000000 // depends-on=2, sync sample
		}
		data := make([]byte, size)
		rng.Read(data)
		out[j] = sampleSpec{dur: uint32(dur), size: size, flags: flags, data: data}
		sum += dur
	}
	// Keep the fragment's total duration exactly on the drifted schedule.
	out[framesPerFrag-1].dur += uint32(int64(fragDelta) - int64(sum))
	return out
}

func cloneSamples(src []sampleSpec) []sampleSpec {
	out := make([]sampleSpec, len(src))
	for i, s := range src {
		data := make([]byte, len(s.data))
		copy(data, s.data)
		out[i] = sampleSpec{dur: s.dur, size: s.size, flags: s.flags, data: data}
	}
	return out
}

// genAudioFragment builds 1s of PCM16: 46 samples of 1024 + one of 896.
// Fragments 2,3 of every 6 are pure silence.
func genAudioFragment(fragIdx int) []sampleSpec {
	silent := fragIdx%6 == 2 || fragIdx%6 == 3
	durs := make([]uint32, 47)
	for i := range durs {
		durs[i] = 1024
	}
	durs[46] = 896
	out := make([]sampleSpec, 0, 47)
	offset := 0
	for _, dur := range durs {
		data := make([]byte, 2*dur)
		if !silent {
			for k := uint32(0); k < dur; k++ {
				g := fragIdx*audioFragDur + offset + int(k)
				v := int16(0.3 * 32767 * math.Sin(2*math.Pi*440*float64(g)/audioFragDur))
				binary.LittleEndian.PutUint16(data[2*k:], uint16(v))
			}
		}
		out = append(out, sampleSpec{dur: dur, size: 2 * dur, flags: 0x2000000, data: data})
		offset += int(dur)
	}
	return out
}

// capturePacket wraps the 33-bit capture clock for video fragment i in an
// RTP packet payload (big-endian uint64), giving the pipeline a drift-free
// reference clock that wraps at opts.WrapAt.
func capturePacket(opts Options, i int) []byte {
	ref := uint64(i) * videoFragDur
	raw := ref
	if opts.WrapAt > 0 {
		start := opts.WrapAt - videoFragDur/2 // wrap mid-stream
		raw = (start + ref) % opts.WrapAt
	}
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, raw)
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Timestamp: uint32(raw), SSRC: 0x12787},
		Payload: payload,
	}
	b, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}
