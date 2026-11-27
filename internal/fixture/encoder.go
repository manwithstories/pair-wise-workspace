// Package fixture generates fully synthetic but standards-valid media
// segments (H.264 video + AAC audio) plus HLS and DASH playlists. No external
// assets are referenced; every frame and sample is produced deterministically
// so repeated runs yield byte-identical outputs and stable fingerprints.
package fixture

import (
	"fmt"
	"sort"
	"sync"

	"github.com/asticode/go-astiav"
)

// warmupOnce guards the one-time encoder warm-up. FFmpeg's libx264 and native
// AAC encoders perform lazy per-configuration initialisation whose first output
// differs from later, otherwise identical, encodes. Running one throwaway
// encode for every (layout, rate, colour) configuration the fixture uses moves
// every subsequent segment onto the stable, byte-reproducible path.
var warmupOnce sync.Once
var warmupErr error

// WarmUp performs the one-time encoder warm-up. It is safe to call repeatedly
// and is invoked automatically by EncodeSegment.
func WarmUp() error {
	warmupOnce.Do(func() {
		// Cover the full cross-product of every parameter the fixture varies,
		// so the first real encode of ANY configuration is already on the
		// stable reproducible path regardless of call/test ordering.
		rates := []int{44100, 48000}
		chans := []int{1, 2, 6}
		colors := []ColorTag{ColorBT709, ColorBT2020}
		idx := 0
		for _, rate := range rates {
			for _, ch := range chans {
				for _, col := range colors {
					cfg := SegmentSpec{
						Width: 64, Height: 36, FrameRate: 25,
						SampleRate: rate, Channels: ch,
						DurationSec: 0.2, ColorTag: col, SegmentIndex: idx,
					}
					if _, err := encodeSegment(cfg); err != nil {
						warmupErr = fmt.Errorf("fixture: warm-up config %d: %w", idx, err)
						return
					}
					idx++
				}
			}
		}
	})
	return warmupErr
}

// SegmentSpec configures one encoded segment.
type SegmentSpec struct {
	Width      int
	Height     int
	FrameRate  int // frames per second
	SampleRate int
	Channels   int
	DurationSec float64
	// SegmentIndex drives the deterministic content generator.
	SegmentIndex int
	// SilentAudio zeroes the audio waveform (used to create real silence).
	SilentAudio bool
	// ColorTag selects the colour metadata written on the video stream.
	ColorTag ColorTag
}

// ColorTag enumerates the colour metadata variants injected for conflict tests.
type ColorTag int

const (
	ColorBT709 ColorTag = iota
	ColorBT2020
)

// EncodeSegment synthesises and muxes one self-contained MPEG-TS segment into
// memory and returns its bytes. A fresh encoder/muxer is used per segment so
// every segment is independently decodable and timestamps start at zero. It
// guarantees byte-reproducible output by ensuring the one-time encoder warm-up
// has completed first.
func EncodeSegment(spec SegmentSpec) ([]byte, error) {
	if err := WarmUp(); err != nil {
		return nil, err
	}
	return encodeSegment(spec)
}

// encodeSegment contains the actual encoding logic without warm-up handling so
// WarmUp can invoke it without recursion.
func encodeSegment(spec SegmentSpec) ([]byte, error) {
	sink := &memSink{}
	ioCtx, err := astiav.AllocIOContext(4096, true, nil,
		sink.Seek,
		func(b []byte) (int, error) {
			return sink.Write(b)
		})
	if err != nil {
		return nil, fmt.Errorf("fixture: alloc io: %w", err)
	}
	defer ioCtx.Free()

	fc, err := astiav.AllocOutputFormatContext(nil, "mpegts", "")
	if err != nil {
		return nil, fmt.Errorf("fixture: alloc output: %w", err)
	}
	defer fc.Free()
	fc.SetPb(ioCtx)

	vEnc, vStream, err := addVideoStream(fc, spec)
	if err != nil {
		return nil, err
	}
	defer vEnc.Free()
	aEnc, aStream, err := addAudioStream(fc, spec)
	if err != nil {
		return nil, err
	}
	defer aEnc.Free()

	if err := fc.WriteHeader(nil); err != nil {
		return nil, fmt.Errorf("fixture: header: %w", err)
	}
	if err := writeVideo(fc, vEnc, vStream, spec); err != nil {
		return nil, fmt.Errorf("fixture: video: %w", err)
	}
	if err := writeAudio(fc, aEnc, aStream, spec); err != nil {
		return nil, fmt.Errorf("fixture: audio: %w", err)
	}
	if err := fc.WriteTrailer(); err != nil {
		return nil, fmt.Errorf("fixture: trailer: %w", err)
	}
	return sink.Bytes(), nil
}

func addVideoStream(fc *astiav.FormatContext, spec SegmentSpec) (*astiav.CodecContext, *astiav.Stream, error) {
	codec := astiav.FindEncoder(astiav.CodecIDH264)
	if codec == nil {
		return nil, nil, fmt.Errorf("fixture: h264 encoder unavailable")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, nil, fmt.Errorf("fixture: alloc video ctx")
	}
	cc.SetWidth(spec.Width)
	cc.SetHeight(spec.Height)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(astiav.NewRational(1, spec.FrameRate))
	cc.SetFramerate(astiav.NewRational(spec.FrameRate, 1))
	// Force an IDR at the segment head so each segment is independently valid.
	cc.SetGopSize(spec.FrameRate)
	cc.SetMaxBFrames(0)
	// Single-threaded encoding is required for byte-deterministic output;
	// libx264 frame threading can otherwise reorder work non-deterministically.
	cc.SetThreadCount(1)
	cc.SetThreadType(astiav.ThreadTypeSlice)
	applyColor(cc, spec.ColorTag)
	if err := setPrivate(cc, map[string]string{
		"preset": "medium",
	}); err != nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: video options: %w", err)
	}
	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: open video: %w", err)
	}
	st := fc.NewStream(nil)
	if st == nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: new video stream")
	}
	if err := st.CodecParameters().FromCodecContext(cc); err != nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: video params: %w", err)
	}
	st.SetTimeBase(cc.TimeBase())
	return cc, st, nil
}

func addAudioStream(fc *astiav.FormatContext, spec SegmentSpec) (*astiav.CodecContext, *astiav.Stream, error) {
	codec := astiav.FindEncoder(astiav.CodecIDAac)
	if codec == nil {
		return nil, nil, fmt.Errorf("fixture: aac encoder unavailable")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, nil, fmt.Errorf("fixture: alloc audio ctx")
	}
	cc.SetSampleRate(spec.SampleRate)
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	cc.SetChannelLayout(audioLayout(spec.Channels))
	cc.SetTimeBase(astiav.NewRational(1, spec.SampleRate))
	cc.SetThreadCount(1)
	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: open audio: %w", err)
	}
	st := fc.NewStream(nil)
	if st == nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: new audio stream")
	}
	if err := st.CodecParameters().FromCodecContext(cc); err != nil {
		cc.Free()
		return nil, nil, fmt.Errorf("fixture: audio params: %w", err)
	}
	st.SetTimeBase(cc.TimeBase())
	return cc, st, nil
}

func audioLayout(channels int) astiav.ChannelLayout {
	switch channels {
	case 1:
		return astiav.ChannelLayoutMono
	case 6:
		return astiav.ChannelLayout5Point1
	default:
		return astiav.ChannelLayoutStereo
	}
}

// setPrivate writes encoder-private options (e.g. libx264 tuning) before the
// codec is opened. Values are applied in sorted key order for determinism.
func setPrivate(cc *astiav.CodecContext, opts map[string]string) error {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := cc.PrivateData().Options().Set(k, opts[k], 0); err != nil {
			return fmt.Errorf("option %s=%s: %w", k, opts[k], err)
		}
	}
	return nil
}
