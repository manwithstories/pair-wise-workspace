package audio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os/exec"

	"medpipe/internal/clock"
)

// CanonicalRate is the single PCM rate every supported track is resampled
// to before analysis, keeping windows comparable across devices.
const CanonicalRate = 48000

// blockSamples bounds the PCM chunk held in memory at any one time.
const blockSamples = 8192

// SupportedSampleRate reports whether a declared audio rate can be mapped.
// The mobile contract covers 44.1/48 kHz; anything else stops the track.
func SupportedSampleRate(rate int) bool {
	switch rate {
	case 44100, 48000:
		return true
	default:
		return false
	}
}

// channelLayout maps a declared channel count to an explicit FFmpeg layout.
func channelLayout(channels int) (string, error) {
	switch channels {
	case 1:
		return "mono", nil
	case 2:
		return "stereo", nil
	case 6:
		return "5.1", nil
	case 8:
		return "7.1", nil
	default:
		return "", clock.E(clock.KindUnsupported, "audio.decode",
			clock.WithDetail("no channel layout mapping for %d channels", channels))
	}
}

// DecodeOptions selects one audio stream and its resampling target.
type DecodeOptions struct {
	StreamIndex int // 0-based audio output index within the file
	Channels    int
	SourceRate  int
	TargetRate  int
}

// AnalyzeSilence decodes the selected audio track to mono s16le PCM,
// resamples to the target rate and drives a Detector through a bounded
// streaming reader. Memory stays flat regardless of clip length.
func AnalyzeSilence(ctx context.Context, path string, opt DecodeOptions) (*Detector, error) {
	if !SupportedSampleRate(opt.SourceRate) {
		return nil, clock.E(clock.KindUnsupported, "audio.decode",
			clock.WithDetail("sample rate %d cannot be mapped (44100/48000 only)",
				opt.SourceRate))
	}
	if _, err := channelLayout(opt.Channels); err != nil {
		return nil, err
	}
	if opt.TargetRate == 0 {
		opt.TargetRate = CanonicalRate
	}
	cfg := DefaultSilenceConfig(opt.TargetRate)
	det, err := NewDetector(cfg)
	if err != nil {
		return nil, err
	}
	err = DecodeStream(ctx, path, opt, func(block []int16) error {
		det.Push(block)
		return nil
	})
	if err != nil {
		return nil, err
	}
	det.Flush()
	return det, nil
}

// DecodeStream runs ffmpeg and streams mono s16le PCM to onBlock. The
// process is killed when ctx is canceled or the callback returns an error.
func DecodeStream(ctx context.Context, path string, opt DecodeOptions, onBlock func([]int16) error) error {
	layout, err := channelLayout(opt.Channels)
	if err != nil {
		return err
	}
	target := opt.TargetRate
	if target == 0 {
		target = CanonicalRate
	}
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", path,
		"-map", streamSpec(opt.StreamIndex),
		"-vn",
		// Explicit layout mapping (validated above), rational resampling to
		// the canonical rate, then a deterministic downmix to mono.
		"-af", "aformat=channel_layouts=" + layout + ":sample_rates=" + itoa(target) +
			",aformat=channel_layouts=mono",
		"-ar", itoa(target),
		"-f", "s16le", "-acodec", "pcm_s16le",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if ctx.Err() != nil {
			return clock.E(clock.KindCanceled, "audio.decode", clock.WithWrap(ctx.Err()))
		}
		return clock.E(clock.KindOther, "audio.decode", clock.WithWrap(err))
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return clock.E(clock.KindCanceled, "audio.decode", clock.WithWrap(ctx.Err()))
		}
		return clock.E(clock.KindOther, "audio.decode",
			clock.WithDetail("cannot start ffmpeg"), clock.WithWrap(err))
	}
	readErr := streamPCM(stdout, onBlock)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return clock.E(clock.KindCanceled, "audio.decode", clock.WithWrap(ctx.Err()))
	}
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return clock.E(clock.KindCorrupt, "audio.decode",
			clock.WithDetail("ffmpeg failed: %s", tail(stderr.Bytes())),
			clock.WithWrap(waitErr))
	}
	return nil
}

func streamPCM(r io.Reader, onBlock func([]int16) error) error {
	br := bufio.NewReaderSize(r, blockSamples*2)
	buf := make([]int16, blockSamples)
	raw := make([]byte, blockSamples*2)
	for {
		n, err := io.ReadFull(br, raw)
		if n > 0 {
			count := n / 2
			for i := 0; i < count; i++ {
				buf[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
			}
			if cbErr := onBlock(buf[:count]); cbErr != nil {
				return cbErr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return clock.E(clock.KindOther, "audio.decode", clock.WithWrap(err))
		}
	}
}

func streamSpec(index int) string {
	return "0:a:" + itoa(index)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func tail(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(bytes.TrimSpace(b))
}
