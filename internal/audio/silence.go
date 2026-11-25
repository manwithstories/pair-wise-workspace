// Package audio decodes supported audio and detects 20 ms silent windows.
package audio

import (
	"math"

	"github.com/pion/opus"

	"mediaproc/internal/demux"
	"mediaproc/internal/ebml"
	"mediaproc/internal/model"
)

const (
	sampleRate48k = 48000
	windowSamples = sampleRate48k / 50 // 20 ms
)

// Window is one 20 ms RMS analysis interval.
type Window struct {
	TrackNumber uint64
	StartNS     model.Nanoseconds
	DurationNS  model.Nanoseconds
	RMS         float64
	Silent      bool
}

// TrackError reports an unsupported or undecodable audio track. Original
// blocks remain in their segments when this is reported.
type TrackError struct {
	TrackNumber uint64
	CodecID     string
	Offset      int64
	Reason      string
	Err         error
}

func (e *TrackError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

// Detector performs per-track Opus decoding and hysteresis silence detection.
type Detector struct {
	Window        model.Nanoseconds
	SilenceEnter  float64
	VoiceExit     float64
	decoders      map[uint64]*opus.Decoder
	pcm           map[uint64][]float32
	state         map[uint64]bool
	reported      map[uint64]bool
	TrackErrors   []TrackError
	SilentWindows int
	Windows       []Window
}

// NewDetector creates a detector with conservative RMS hysteresis.
func NewDetector() *Detector {
	return &Detector{
		Window:       20 * model.Millisecond,
		SilenceEnter: 0.0015,
		VoiceExit:    0.006,
		decoders:     map[uint64]*opus.Decoder{},
		pcm:          map[uint64][]float32{},
		state:        map[uint64]bool{},
		reported:     map[uint64]bool{},
	}
}

func (d *Detector) decoderFor(trackNumber uint64, t *demux.Track) (*opus.Decoder, int, error) {
	channels := 1
	if t != nil && t.Channels == 2 {
		channels = 2
	}
	if dec, ok := d.decoders[trackNumber]; ok {
		return dec, channels, nil
	}
	dec, err := opus.NewDecoderWithOutput(sampleRate48k, channels)
	if err != nil {
		return nil, channels, err
	}
	d.decoders[trackNumber] = &dec
	return &dec, channels, nil
}

// AnalyzeBlock decodes Opus audio and analyzes complete 20 ms windows.
// Unsupported codecs produce a TrackError and no decoding error so the caller
// can retain the original block.
func (d *Detector) AnalyzeBlock(b *demux.Block) []Window {
	if b == nil {
		return nil
	}
	if b.Track != nil && b.Track.Type != ebml.TrackTypeAudio {
		return nil
	}
	if b.Track != nil && b.Track.CodecID != "A_OPUS" {
		if !d.reported[b.TrackNumber] {
			d.reported[b.TrackNumber] = true
			d.TrackErrors = append(d.TrackErrors, TrackError{
				TrackNumber: b.TrackNumber,
				CodecID:     b.Track.CodecID,
				Offset:      b.Offset,
				Reason:      "unsupported audio codec; original blocks retained",
			})
		}
		return nil
	}

	dec, channels, err := d.decoderFor(b.TrackNumber, b.Track)
	if err != nil {
		d.appendDecodeError(b, err)
		return nil
	}

	var produced []float32
	for _, frame := range b.Frames {
		out := make([]float32, 5760*channels)
		n, derr := dec.DecodeToFloat32(frame, out)
		if derr != nil {
			d.appendDecodeError(b, derr)
			continue
		}
		if n <= 0 {
			continue
		}
		frameSamples := out[:n*channels]
		if channels == 2 {
			mono := make([]float32, n)
			for i := 0; i < n; i++ {
				mono[i] = (frameSamples[2*i] + frameSamples[2*i+1]) * 0.5
			}
			frameSamples = mono
		}
		produced = append(produced, frameSamples...)
	}
	if len(produced) == 0 {
		return nil
	}

	track := b.TrackNumber
	d.pcm[track] = append(d.pcm[track], produced...)
	pcm := d.pcm[track]
	var windows []Window
	windowIndex := 0
	for len(pcm) >= windowSamples {
		chunk := pcm[:windowSamples]
		rms := RMS(chunk)
		silent := d.classify(track, rms)
		w := Window{
			TrackNumber: track,
			StartNS:     b.TimeNS + model.Nanoseconds(windowIndex)*d.Window,
			DurationNS:  d.Window,
			RMS:         rms,
			Silent:      silent,
		}
		windows = append(windows, w)
		d.Windows = append(d.Windows, w)
		if silent {
			d.SilentWindows++
		}
		windowIndex++
		pcm = pcm[windowSamples:]
	}
	d.pcm[track] = pcm
	return windows
}

func (d *Detector) appendDecodeError(b *demux.Block, err error) {
	if d.reported[b.TrackNumber] {
		return
	}
	d.reported[b.TrackNumber] = true
	codec := ""
	if b.Track != nil {
		codec = b.Track.CodecID
	}
	d.TrackErrors = append(d.TrackErrors, TrackError{
		TrackNumber: b.TrackNumber,
		CodecID:     codec,
		Offset:      b.Offset,
		Reason:      "Opus decode failed; original blocks retained",
		Err:         err,
	})
}

func (d *Detector) classify(track uint64, rms float64) bool {
	silent := d.state[track]
	if !silent && rms < d.SilenceEnter {
		silent = true
	} else if silent && rms > d.VoiceExit {
		silent = false
	}
	d.state[track] = silent
	return silent
}

// RMS computes root-mean-square amplitude for float32 PCM.
func RMS(pcm []float32) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, x := range pcm {
		v := float64(x)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(pcm)))
}
