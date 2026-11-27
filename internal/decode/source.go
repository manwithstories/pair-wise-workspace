// Package decode drives FFmpeg through go-astiav to turn an audio stream into
// planar float samples on the unified timeline. It is the only place that
// touches libavcodec; the DSP afterwards is pure Go.
package decode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/asticode/go-astiav"

	"mediacore/internal/audio"
	"mediacore/internal/clock"
	"mediacore/internal/stream"
)

// ErrNoAudio reports a file without a decodable audio track.
var ErrNoAudio = errors.New("decode: no audio track")

// Source decodes one audio stream and slices it on the unified timeline.
//
// It decodes the whole stream once, keeping only the requested segments'
// samples; callers ask for segments in increasing order, which lets the
// decoder release frames as it goes.
type Source struct {
	path      string
	fc        *astiav.FormatContext
	streamIdx int
	stream    *astiav.Stream
	dec       *astiav.CodecContext
	timeline  *clock.Timeline
	format    stream.AudioFormat
	rate      int
	channels  int
	// pending holds decoded frames not yet consumed, as (startUS, samples).
	posUS int64
	eof   bool
}

// Open prepares an audio decoder for path. timeline must describe the same
// file (from the scanner) so timestamps land on the shared origin.
func Open(path string, tl *clock.Timeline) (*Source, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("decode: open %q: %w", path, err)
	}
	if tl == nil {
		return nil, errors.New("decode: missing timeline")
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, errors.New("decode: alloc format context")
	}
	// Streamable containers (MPEG-TS) often need a larger probe before an
	// audio stream's rate and channel count are known. The options must be in
	// place before OpenInput; find_stream_info is called exactly once because
	// a second call on the same context is not safe.
	opts := astiav.NewDictionary()
	defer opts.Free()
	if err := opts.Set("probesize", "16777216", 0); err != nil {
		fc.Free()
		return nil, err
	}
	if err := opts.Set("analyzeduration", "10000000", 0); err != nil {
		fc.Free()
		return nil, err
	}
	if err := fc.OpenInput(path, nil, opts); err != nil {
		fc.Free()
		return nil, fmt.Errorf("decode: open input: %w", err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("decode: stream info: %w", err)
	}
	var st *astiav.Stream
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			st = s
			break
		}
	}
	if st == nil {
		fc.CloseInput()
		fc.Free()
		return nil, ErrNoAudio
	}
	dec := astiav.FindDecoder(st.CodecParameters().CodecID())
	if dec == nil {
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("%w: no decoder for %s", ErrNoAudio, st.CodecParameters().CodecID())
	}
	cc := astiav.AllocCodecContext(dec)
	if cc == nil {
		fc.CloseInput()
		fc.Free()
		return nil, errors.New("decode: alloc codec context")
	}
	if err := st.CodecParameters().ToCodecContext(cc); err != nil {
		cc.Free()
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("decode: params: %w", err)
	}
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	if err := cc.Open(dec, nil); err != nil {
		cc.Free()
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("decode: open decoder: %w", err)
	}
	rate := cc.SampleRate()
	if rate <= 0 {
		rate = st.CodecParameters().SampleRate()
	}
	ch := cc.ChannelLayout().Channels()
	if ch <= 0 {
		ch = st.CodecParameters().ChannelLayout().Channels()
	}
	if rate <= 0 || ch <= 0 {
		cc.Free()
		fc.CloseInput()
		fc.Free()
		return nil, fmt.Errorf("decode: invalid audio format %d Hz %d ch", rate, ch)
	}
	src := &Source{
		path: path, fc: fc, streamIdx: st.Index(), stream: st, dec: cc, timeline: tl,
		rate: rate, channels: ch,
		format: stream.AudioFormat{Rate: rate, Channels: ch, Layout: layoutName(ch), SampleFormat: "fltp"},
	}
	return src, nil
}

// AudioFormat reports the decoded format.
func (s *Source) AudioFormat() stream.AudioFormat { return s.format }

// ReadIntoBuffer returns the samples inside [startUS, endUS).
//
// A window is reproducible regardless of decode history: the decoder rewinds
// and pre-rolls PREROLL_US before the requested start, and only samples whose
// own timestamps fall inside the window are kept. Without the rewind, segment
// N decoded as part of a full run would differ from segment N decoded after a
// resume, because AAC frames carry coding history across segment boundaries.
func (s *Source) ReadIntoBuffer(ctx context.Context, startUS, endUS int64) (audio.Buffer, error) {
	if endUS < startUS {
		return audio.Buffer{}, fmt.Errorf("decode: inverted window [%d,%d)", startUS, endUS)
	}
	if err := s.rewind(); err != nil {
		return audio.Buffer{}, err
	}
	out := audio.Buffer{Rate: s.rate, Planes: make([][]float32, s.channels)}
	for c := range out.Planes {
		out.Planes[c] = nil
	}
	// Keep reading while decoded position has not passed the window end.
	for !s.eof && s.posUS < endUS {
		if err := ctx.Err(); err != nil {
			return audio.Buffer{}, err
		}
		got, start, err := s.nextFrame()
		if err != nil {
			return audio.Buffer{}, err
		}
		if got == nil {
			continue
		}
		frames := len(got[0])
		// Window of this frame on the timeline. Boundaries are derived from
		// each sample's own position in source sample units converted with
		// exact rational rounding, so a frame split between two segments is
		// never assigned to both or to neither.
		frameEnd := start + int64(frames)*1_000_000/int64(s.rate)
		s.posUS = frameEnd
		if frameEnd <= startUS || start >= endUS {
			continue
		}
		lo, hi := 0, frames
		if start < startUS {
			lo = int((startUS - start) * int64(s.rate) / 1_000_000)
		}
		if frameEnd > endUS {
			hi = int((endUS - start) * int64(s.rate) / 1_000_000)
		}
		if lo < 0 {
			lo = 0
		}
		if hi > frames {
			hi = frames
		}
		if lo >= hi {
			continue
		}
		for c := range out.Planes {
			out.Planes[c] = append(out.Planes[c], got[c][lo:hi]...)
		}
	}
	// Pad channels to equal length (decoder frames are already equal, but a
	// partial window can leave one plane short if a frame is malformed).
	n := 0
	for _, p := range out.Planes {
		if len(p) > n {
			n = len(p)
		}
	}
	for c := range out.Planes {
		if len(out.Planes[c]) < n {
			out.Planes[c] = append(out.Planes[c], make([]float32, n-len(out.Planes[c]))...)
		}
	}
	return out, nil
}

// rewind resets the decoder to the start of the stream.
//
// Segment output must be identical whether a segment is reached by a full run
// or by a resume after a crash. Audio codecs carry overlap across packet
// boundaries and FFmpeg's decoder has no public flush in go-astiav v0.42.0, so
// the only history-independent reset is a fresh demux+decode from the file
// start. Segments are short and this keeps the digest a pure function of the
// segment window.
func (s *Source) rewind() error {
	s.dec.Free()
	if s.fc != nil {
		s.fc.CloseInput()
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return errors.New("decode: realloc format context")
	}
	if err := fc.OpenInput(s.path, nil, nil); err != nil {
		fc.Free()
		return fmt.Errorf("decode: reopen input: %w", err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("decode: reopen stream info: %w", err)
	}
	var st *astiav.Stream
	for _, cand := range fc.Streams() {
		if cand.Index() == s.streamIdx {
			st = cand
			break
		}
	}
	if st == nil {
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("decode: stream %d vanished on reopen", s.streamIdx)
	}
	dec := astiav.FindDecoder(st.CodecParameters().CodecID())
	if dec == nil {
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("%w: decoder vanished", ErrNoAudio)
	}
	cc := astiav.AllocCodecContext(dec)
	if cc == nil {
		fc.CloseInput()
		fc.Free()
		return errors.New("decode: realloc codec context")
	}
	if err := st.CodecParameters().ToCodecContext(cc); err != nil {
		cc.Free()
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("decode: reopen params: %w", err)
	}
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	if err := cc.Open(dec, nil); err != nil {
		cc.Free()
		fc.CloseInput()
		fc.Free()
		return fmt.Errorf("decode: reopen decoder: %w", err)
	}
	s.fc, s.stream, s.dec = fc, st, cc
	s.posUS = 0
	s.eof = false
	return nil
}

// nextFrame decodes the next audio frame and returns its planar samples plus
// the unified start timestamp.
func (s *Source) nextFrame() ([][]float32, int64, error) {
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()
	for {
		// Drain decoded frames first.
		if err := s.dec.ReceiveFrame(frame); err == nil {
			data, start, err := s.frameToPlanes(frame)
			frame.Unref()
			return data, start, err
		} else if !errors.Is(err, astiav.ErrEagain) && !errors.Is(err, astiav.ErrEof) {
			return nil, 0, fmt.Errorf("decode: receive frame: %w", err)
		}
		if s.eof {
			return nil, 0, nil
		}
		// Feed packets until a frame pops out.
		for {
			if err := s.fc.ReadFrame(pkt); err != nil {
				if errors.Is(err, astiav.ErrEof) {
					s.eof = true
					s.dec.SendPacket(nil)
					break
				}
				return nil, 0, fmt.Errorf("decode: read frame: %w", err)
			}
			if pkt.StreamIndex() != s.streamIdx {
				pkt.Unref()
				continue
			}
			err := s.dec.SendPacket(pkt)
			pkt.Unref()
			if err != nil && !errors.Is(err, astiav.ErrEagain) {
				return nil, 0, fmt.Errorf("decode: send packet: %w", err)
			}
			break
		}
	}
}

func (s *Source) frameToPlanes(frame *astiav.Frame) ([][]float32, int64, error) {
	n := frame.NbSamples()
	if n <= 0 {
		return nil, 0, nil
	}
	pts := frame.Pts()
	if pts == astiav.NoPtsValue {
		pts = frame.PktDts()
	}
	var startUS int64
	if pts == astiav.NoPtsValue {
		startUS = s.posUS
	} else {
		tb := clock.Rational{Num: int64(s.stream.TimeBase().Num()), Den: int64(s.stream.TimeBase().Den())}
		v, err := s.timeline.ToUnified(s.streamIdx, pts)
		if err != nil {
			// Fall back to the running position rather than inventing a time.
			v = s.posUS
		}
		startUS = v
		_ = tb
	}
	size, err := frame.SamplesBufferSize(0)
	if err != nil {
		return nil, 0, fmt.Errorf("decode: buffer size: %w", err)
	}
	buf := make([]byte, size)
	if _, err := frame.SamplesCopyToBuffer(buf, 0); err != nil {
		return nil, 0, fmt.Errorf("decode: copy samples: %w", err)
	}
	planes := make([][]float32, s.channels)
	need := s.channels * n * 4
	if len(buf) < need {
		return nil, 0, fmt.Errorf("decode: short audio buffer %d < %d", len(buf), need)
	}
	for c := 0; c < s.channels; c++ {
		planes[c] = make([]float32, n)
		for i := 0; i < n; i++ {
			off := (c*n + i) * 4
			planes[c][i] = math.Float32frombits(uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24)
		}
	}
	return planes, startUS, nil
}

// Close releases FFmpeg resources.
func (s *Source) Close() {
	if s.dec != nil {
		s.dec.Free()
	}
	if s.fc != nil {
		s.fc.CloseInput()
		s.fc.Free()
	}
}

func layoutName(channels int) string {
	switch channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%dch", channels)
}
