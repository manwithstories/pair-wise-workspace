package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"time"

	"mediapipeline/internal/audio"
	"mediapipeline/internal/container"
	"mediapipeline/internal/index"
	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/stream"
	"mediapipeline/internal/testdata"
)

// segmentAcc accumulates the ordered packets belonging to one time range and
// holds the audio DSP state so the negotiated spec is applied continuously.
type segmentAcc struct {
	index    int
	startNS  int64
	endNS    int64
	audio    [][]byte
	video    [][]byte
	videoKey []bool
	failures []string
}

// stageConsume applies the unified clock, the audio negotiation and segment
// boundaries, then writes and verifies an MP4 container per segment.
func (r *Runner) stageConsume(
	ctx context.Context,
	in *boundedQueue,
	fx *testdata.Fixture,
	res *Result,
	addFailure func(*mediaerr.Error),
	fatal FatalFunc,
) {
	acc := &segmentAcc{index: 0, startNS: 0, endNS: r.opts.SegmentNS}
	dsp, merr := newDSP(r.opts.Audio)
	if merr != nil {
		fatal(merr)
		return
	}
	for {
		p, ok, rerr := in.Receive(ctx)
		if rerr != nil {
			fatal(rerr)
			return
		}
		if !ok {
			break
		}
		if r.opts.ConsumerDelayNS > 0 {
			// Simulate a slow consumer so back pressure propagates upstream;
			// an interruptible sleep keeps cancellation responsive.
			timer := time.NewTimer(time.Duration(r.opts.ConsumerDelayNS))
			select {
			case <-ctx.Done():
				timer.Stop()
				fatal(mediaerr.AsError(mediaerr.StagePipeline, ctx.Err()))
				return
			case <-timer.C:
			}
		}
		ns, terr := r.mapTime(p)
		if terr != nil {
			acc.failures = append(acc.failures, terr.Error())
			addFailure(terr)
		}
		// Close the current segment once the packet crosses its end.
		for ns >= acc.endNS {
			r.emitSegment(acc, dsp, res, addFailure)
			acc = r.nextAcc(acc)
		}
		if p.PayloadType == 97 {
			acc.video = append(acc.video, append([]byte(nil), p.Payload...))
			acc.videoKey = append(acc.videoKey, len(p.Payload) > 0 && p.Payload[0] == 0x65)
		} else {
			processed, perr := dsp.processAudio(p.Payload)
			if perr != nil {
				acc.failures = append(acc.failures, perr.Error())
				addFailure(perr)
			}
			acc.audio = append(acc.audio, processed)
		}
	}
	// Flush resampler tail and emit the final partial segment if it holds data.
	tail, ferr := dsp.flush()
	if ferr != nil {
		addFailure(ferr)
	}
	if len(tail) > 0 {
		acc.audio = append(acc.audio, tail)
	}
	if len(acc.audio) > 0 || len(acc.video) > 0 || len(acc.failures) > 0 {
		r.emitSegment(acc, dsp, res, addFailure)
	}
}

// mapTime converts a packet's RTP timestamp onto the track's nanosecond axis.
func (r *Runner) mapTime(p *stream.Packet) (int64, *mediaerr.Error) {
	r.stateMu.RLock()
	tl, ok := r.tlines[p.SSRC]
	r.stateMu.RUnlock()
	if !ok {
		return 0, mediaerr.New(mediaerr.StageClock, mediaerr.KindInvalidInput,
			"packet arrived before timeline anchor").WithTrack(p.SSRC).WithSeq(p.Sequence)
	}
	return tl.ToNS(p.Timestamp)
}

func (r *Runner) nextAcc(prev *segmentAcc) *segmentAcc {
	return &segmentAcc{
		index:   prev.index + 1,
		startNS: prev.endNS,
		endNS:   prev.endNS + r.opts.SegmentNS,
	}
}

// emitSegment builds, writes and verifies one MP4 segment, recording its hash
// and round-trip metrics. A failure in one segment is flagged but does not
// prevent later segments from being processed.
func (r *Runner) emitSegment(
	acc *segmentAcc,
	dsp *dspChain,
	res *Result,
	addFailure func(*mediaerr.Error),
) {
	out := SegmentResult{
		Index: acc.index, StartNS: acc.startNS, EndNS: acc.endNS,
		FailureReasons: acc.failures, PartialFailure: len(acc.failures) > 0,
		AudioSamples: len(acc.audio), VideoSamples: len(acc.video),
	}
	tracks, merr := r.buildTracks(acc, dsp)
	if merr != nil {
		out.PartialFailure = true
		out.FailureReasons = append(out.FailureReasons, merr.Error())
		addFailure(merr)
		res.Segments = append(res.Segments, out)
		return
	}
	if len(tracks) == 0 {
		// Nothing media-bearing to containerize; still report the empty span.
		out.FailureReasons = append(out.FailureReasons, "empty segment")
		out.PartialFailure = true
		res.Segments = append(res.Segments, out)
		return
	}
	data, werr := container.Write(tracks)
	if werr != nil {
		out.PartialFailure = true
		out.FailureReasons = append(out.FailureReasons, werr.Error())
		addFailure(werr)
		res.Segments = append(res.Segments, out)
		return
	}
	sum := sha256.Sum256(data)
	out.SHA256 = hex.EncodeToString(sum[:])
	parsed, perr := container.Parse(data)
	if perr != nil {
		out.PartialFailure = true
		out.FailureReasons = append(out.FailureReasons, perr.Error())
		addFailure(perr)
		res.Segments = append(res.Segments, out)
		return
	}
	out.RoundTrip = container.RoundTripConsistency(tracks, parsed)
	out.KeyframeHitRate = r.keyframeRate(tracks, parsed)
	res.Segments = append(res.Segments, out)
}

// buildTracks turns the accumulated packets into container track configs.
func (r *Runner) buildTracks(acc *segmentAcc, dsp *dspChain) ([]container.TrackConfig, *mediaerr.Error) {
	var tracks []container.TrackConfig
	if len(acc.audio) > 0 {
		tracks = append(tracks, container.TrackConfig{
			TrackID: 1, Timescale: uint32(r.opts.Audio.TargetRate),
			IsVideo: false, Delta: uint32(r.opts.Audio.TargetRate) / 100,
			Samples: acc.audio,
		})
	}
	if len(acc.video) > 0 {
		sizes := make([]int, len(acc.video))
		for i, v := range acc.video {
			sizes[i] = len(v)
		}
		idx, merr := index.Build(2, keyframeSamples(acc.videoKey, sizes))
		if merr != nil && merr.Kind != mediaerr.KindNoKeyframe {
			return nil, merr
		}
		tracks = append(tracks, container.TrackConfig{
			TrackID: 2, Timescale: 90000, IsVideo: true, Delta: 3600,
			Samples: acc.video, Keyframes: idx,
		})
	}
	return tracks, nil
}

func keyframeSamples(keys []bool, sizes []int) []index.Sample {
	out := make([]index.Sample, len(keys))
	for i := range keys {
		out[i] = index.Sample{IsKeyframe: keys[i], Size: sizes[i]}
	}
	return out
}

// keyframeRate measures the stss hit rate for a segment. A video segment
// containing no keyframe correctly omits the stss box and round-trips
// perfectly, so it contributes a vacuous 1.0; the rate is only measured over
// segments that actually carry a keyframe index. The no-keyframe *stream*
// condition is reported separately by the index/verify checks.
func (r *Runner) keyframeRate(tracks []container.TrackConfig, parsed *container.ParsedFile) float64 {
	for i, tc := range tracks {
		if !tc.IsVideo || i >= len(parsed.Tracks) {
			continue
		}
		if tc.Keyframes == nil || len(tc.Keyframes.Entries()) == 0 {
			return 1.0
		}
		_, rate := tc.Keyframes.HitRate(parsed.Tracks[i].KeyframeNums)
		return rate
	}
	return 1.0 // audio-only or no video index: consistency is vacuously perfect
}

// dspChain holds the resampler, layout normalizer, loudness meter and limiter
// as one continuous, stateful audio chain.
type dspChain struct {
	spec      AudioSpec
	resampler *audio.Resampler
	meter     *audio.LoudnessMeter
	limiter   *audio.Limiter
	srcLayout audio.Layout
	dstLayout audio.Layout
}

func newDSP(spec AudioSpec) (*dspChain, *mediaerr.Error) {
	rs, merr := audio.NewResampler(int(spec.ClockRate), spec.TargetRate)
	if merr != nil {
		return nil, merr
	}
	meter, merr := audio.NewLoudnessMeter(spec.WindowSamples)
	if merr != nil {
		return nil, merr
	}
	limiter, merr := audio.NewLimiter(spec.PeakCeiling)
	if merr != nil {
		return nil, merr
	}
	return &dspChain{
		spec: spec, resampler: rs, meter: meter, limiter: limiter,
		srcLayout: audio.LayoutMono, dstLayout: audio.Layout(spec.TargetLayout),
	}, nil
}

// processAudio converts one float32 mono payload into negotiated PCM bytes.
func (d *dspChain) processAudio(payload []byte) ([]byte, *mediaerr.Error) {
	if len(payload)%4 != 0 {
		return nil, mediaerr.New(mediaerr.StageResample, mediaerr.KindIncompleteFrame,
			"audio payload is not a whole number of float32 frames")
	}
	mono := make([]float64, len(payload)/4)
	for i := range mono {
		mono[i] = float64(readFloat32(payload[i*4:]))
	}
	resampled, merr := d.resampler.Process(mono)
	if merr != nil {
		return nil, merr
	}
	laid, merr := audio.Normalize(d.srcLayout, d.dstLayout, resampled)
	if merr != nil {
		return nil, merr
	}
	limited, _ := d.limiter.Limit(laid)
	if _, merr := d.meter.Process(limited); merr != nil {
		return nil, merr
	}
	return float64ToBytes(limited), nil
}

// flush drains resampler tail samples through the remaining chain.
func (d *dspChain) flush() ([]byte, *mediaerr.Error) {
	tail, merr := d.resampler.Flush()
	if merr != nil {
		return nil, merr
	}
	if len(tail) == 0 {
		return nil, nil
	}
	laid, merr := audio.Normalize(d.srcLayout, d.dstLayout, tail)
	if merr != nil {
		return nil, merr
	}
	limited, _ := d.limiter.Limit(laid)
	return float64ToBytes(limited), nil
}

// readFloat32 decodes a little-endian IEEE-754 float32 without math import
// churn at call sites.
func readFloat32(b []byte) float32 {
	bits := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	return math.Float32frombits(bits)
}

func float64ToBytes(samples []float64) []byte {
	out := make([]byte, len(samples)*8)
	for i, v := range samples {
		bits := math.Float64bits(v)
		for j := 0; j < 8; j++ {
			out[i*8+j] = byte(bits >> (8 * j))
		}
	}
	return out
}
