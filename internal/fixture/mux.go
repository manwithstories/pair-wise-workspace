package fixture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"

	"github.com/asticode/go-astiav"

	"mediacore/internal/meta"
)

// generated bundles the streams and structures produced for one fixture file.
type generated struct {
	videoFrames int
	sampleRate  int
	channels    int
	seconds     float64
	records     []meta.Record
	chapters    []meta.Chapter
	subs        int
	sei         int
}

// muxer writes one fixture container. The concrete implementation is chosen
// from Options.Format.
type muxer interface {
	AddVideo(width, height, fps int, codec string) error
	AddAudio(rate, channels int) error
	AddSubtitle() error
	AddChapter(ch meta.Chapter) error
	WriteVideo(ptsUS int64, yuv []byte) error
	WriteAudio(ptsUS int64, samples []byte) error
	WriteSubtitle(ptsUS, durUS int64, text string) error
	Close() error
}

// writer is the container-agnostic fixture writer built on astiav.
type writer struct {
	fc      *astiav.FormatContext
	io      *astiav.IOContext
	path    string
	format  MediaFormat
	video   *streamWriter
	audio   *streamWriter
	sub     *streamWriter
	enc     *astiav.CodecContext
	audioCC *astiav.CodecContext
	closed  bool
	// headerWritten gates WriteTrailer: calling it after a failed header
	// would make FFmpeg dereference a partially initialized context.
	headerWritten bool
}

type streamWriter struct {
	stream *astiav.Stream
	index  int
	tb     astiav.Rational
}

// newWriter creates the output container for opt.
func newWriter(opt Options) (*writer, error) {
	path := opt.Path()
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: create dir: %w", err)
	}
	os.Remove(path)
	fc, err := astiav.AllocOutputFormatContext(nil, string(opt.Format), path)
	if err != nil {
		return nil, fmt.Errorf("fixture: alloc output: %w", err)
	}
	io, err := astiav.OpenIOContext(path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		fc.Free()
		return nil, fmt.Errorf("fixture: open output: %w", err)
	}
	fc.SetPb(io)
	return &writer{fc: fc, io: io, path: path, format: opt.Format}, nil
}

// AddVideo configures the H.264 stream at the requested frame rate.
func (w *writer) AddVideo(width, height, fps int, codec string) error {
	st := w.fc.NewStream(nil)
	st.SetTimeBase(astiav.NewRational(1, 90000))
	enc := astiav.FindEncoderByName("libx264")
	if enc == nil {
		return fmt.Errorf("fixture: libx264 encoder unavailable")
	}
	cc := astiav.AllocCodecContext(enc)
	cc.SetWidth(width)
	cc.SetHeight(height)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(astiav.NewRational(1, 90000))
	cc.SetFramerate(astiav.NewRational(fps, 1))
	cc.SetGopSize(fps)
	cc.SetMaxBFrames(0)
	cc.SetFlags(astiav.NewCodecContextFlags(astiav.CodecContextFlagGlobalHeader))
	// libx264 rejects FFmpeg's default ratecontrol, so set a preset and a
	// constant quality explicitly, before opening the encoder.
	if err := cc.PrivateData().Options().Set("preset", "ultrafast", 0); err != nil {
		cc.Free()
		return fmt.Errorf("fixture: x264 preset: %w", err)
	}
	if err := cc.PrivateData().Options().Set("crf", "32", 0); err != nil {
		cc.Free()
		return fmt.Errorf("fixture: x264 crf: %w", err)
	}
	if err := cc.Open(enc, nil); err != nil {
		cc.Free()
		return fmt.Errorf("fixture: open video encoder: %w", err)
	}
	if err := cc.ToCodecParameters(st.CodecParameters()); err != nil {
		cc.Free()
		return err
	}
	w.video = &streamWriter{stream: st, index: st.Index(), tb: astiav.NewRational(1, 90000)}
	w.enc = cc
	return nil
}

// AddAudio configures the AAC stream.
func (w *writer) AddAudio(rate, channels int) error {
	st := w.fc.NewStream(nil)
	st.SetTimeBase(astiav.NewRational(1, rate))
	enc := astiav.FindEncoderByName("aac")
	if enc == nil {
		return fmt.Errorf("fixture: aac encoder unavailable")
	}
	cc := astiav.AllocCodecContext(enc)
	cc.SetSampleRate(rate)
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	cc.SetTimeBase(astiav.NewRational(1, rate))
	cc.SetBitRate(64000)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	cc.SetFlags(astiav.NewCodecContextFlags(astiav.CodecContextFlagGlobalHeader))
	if err := cc.Open(enc, nil); err != nil {
		cc.Free()
		return fmt.Errorf("fixture: open audio encoder: %w", err)
	}

	if err := cc.ToCodecParameters(st.CodecParameters()); err != nil {
		cc.Free()
		return err
	}
	w.audio = &streamWriter{stream: st, index: st.Index(), tb: astiav.NewRational(1, rate)}
	w.audioCC = cc
	return nil
}

// AddSubtitle configures a mov_text subtitle stream (MP4 only).
func (w *writer) AddSubtitle() error {
	if w.format != FormatMP4 {
		return nil
	}
	st := w.fc.NewStream(nil)
	st.SetTimeBase(astiav.NewRational(1, 1_000_000))
	cp := st.CodecParameters()
	cp.SetCodecID(astiav.CodecIDMovText)
	cp.SetMediaType(astiav.MediaTypeSubtitle)
	cp.SetCodecTag(astiav.CodecTag(0x67337874))
	w.sub = &streamWriter{stream: st, index: st.Index(), tb: astiav.NewRational(1, 1_000_000)}
	return nil
}

// AddDataTrack configures a bin_data stream for timed metadata (MPEG-TS).
func (w *writer) AddDataTrack() (*streamWriter, error) {
	st := w.fc.NewStream(nil)
	st.SetTimeBase(astiav.NewRational(1, 1_000_000))
	cp := st.CodecParameters()
	cp.SetCodecID(astiav.CodecIDBinData)
	cp.SetMediaType(astiav.MediaTypeData)
	sw := &streamWriter{stream: st, index: st.Index(), tb: astiav.NewRational(1, 1_000_000)}
	return sw, nil
}

// WriteData writes one timed metadata sample on the data track.
func (w *writer) WriteData(sw *streamWriter, ptsUS int64, payload []byte) error {
	if sw == nil {
		return nil
	}
	p := astiav.AllocPacket()
	defer p.Free()
	if err := p.FromData(payload); err != nil {
		return err
	}
	p.SetStreamIndex(sw.index)
	tb := sw.stream.TimeBase()
	den := int64(tb.Den())
	if den == 0 {
		den = 1_000_000
	}
	p.SetPts(ptsUS * den / 1_000_000)
	p.SetDts(p.Pts())
	return w.fc.WriteInterleavedFrame(p)
}

// AddChapter appends a chapter before the header is written.
func (w *writer) AddChapter(ch meta.Chapter) error {
	c, err := w.fc.NewChapter()
	if err != nil {
		return fmt.Errorf("fixture: new chapter: %w", err)
	}
	c.SetID(ch.ID)
	c.SetTimeBase(astiav.NewRational(1, 1_000_000))
	c.SetStart(ch.StartUs)
	c.SetEnd(ch.EndUs)
	d := astiav.NewDictionary()
	if err := d.Set("title", ch.Title, 0); err != nil {
		d.Free()
		return err
	}
	c.SetMetadata(d)
	return nil
}

// Start writes the container header; call after all streams are configured.
func (w *writer) Start() error {
	if err := w.fc.WriteHeader(nil); err != nil {
		return fmt.Errorf("fixture: write header: %w", err)
	}
	w.headerWritten = true
	return nil
}

// drain encodes any queued packets for a stream.
func (w *writer) drain(cc *astiav.CodecContext, sw *streamWriter) error {
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	for {
		if err := cc.ReceivePacket(pkt); err != nil {
			// EAGAIN means the encoder needs more input; EOF means it is
			// fully drained. Both end the drain loop.
			if isEOF(err) || errors.Is(err, astiav.ErrEagain) {
				return nil
			}
			return err
		}
		pkt.SetStreamIndex(sw.index)
		pkt.RescaleTs(cc.TimeBase(), sw.stream.TimeBase())
		if err := w.fc.WriteInterleavedFrame(pkt); err != nil {
			pkt.Unref()
			return fmt.Errorf("fixture: write packet: %w", err)
		}
		pkt.Unref()
	}
}

// WriteVideo encodes one YUV420p frame at ptsUS.
func (w *writer) WriteVideo(ptsUS int64, yuv []byte) error {
	if w.enc == nil {
		return fmt.Errorf("fixture: video stream not configured")
	}
	f := astiav.AllocFrame()
	defer f.Free()
	f.SetWidth(w.enc.Width())
	f.SetHeight(w.enc.Height())
	f.SetPixelFormat(astiav.PixelFormatYuv420P)
	f.SetPts(ptsUS * 90000 / 1_000_000)
	if err := f.AllocBuffer(1); err != nil {
		return err
	}
	if err := f.Data().SetBytes(yuv, 1); err != nil {
		return err
	}
	if err := w.enc.SendFrame(f); err != nil {
		return err
	}
	return w.drain(w.enc, w.video)
}

// WriteAudio encodes planar float stereo at ptsUS.
func (w *writer) WriteAudio(ptsUS int64, samples []byte) error {
	if w.audioCC == nil {
		return fmt.Errorf("fixture: audio stream not configured")
	}
	f := astiav.AllocFrame()
	defer f.Free()
	f.SetSampleFormat(astiav.SampleFormatFltp)
	f.SetSampleRate(w.audioCC.SampleRate())
	f.SetChannelLayout(astiav.ChannelLayoutStereo)
	n := len(samples) / 8 // stereo float32
	f.SetNbSamples(n)
	f.SetPts(ptsUS * int64(w.audioCC.SampleRate()) / 1_000_000)
	if err := f.AllocBuffer(1); err != nil {
		return err
	}
	// SetBytes expects the same alignment the buffer was allocated with.
	if err := f.Data().SetBytes(samples, 1); err != nil {
		return err
	}
	if err := w.audioCC.SendFrame(f); err != nil {
		return err
	}
	return w.drain(w.audioCC, w.audio)
}

// WriteSubtitle writes one mov_text sample.
func (w *writer) WriteSubtitle(ptsUS, durUS int64, text string) error {
	if w.sub == nil {
		return nil
	}
	payload, err := meta.EncodeSubtitle("mov_text", text, nil)
	if err != nil {
		return err
	}
	p := astiav.AllocPacket()
	defer p.Free()
	if err := p.FromData(payload); err != nil {
		return err
	}
	p.SetStreamIndex(w.sub.index)
	// All timestamps are expressed in the subtitle stream's time base.
	tb := w.sub.stream.TimeBase()
	den := int64(tb.Den())
	if den == 0 {
		den = 1_000_000
	}
	p.SetPts(ptsUS * den / 1_000_000)
	p.SetDts(p.Pts())
	p.SetDuration(durUS * den / 1_000_000)
	if err := w.fc.WriteInterleavedFrame(p); err != nil {
		return err
	}
	return nil
}

// Close flushes encoders and the container.
func (w *writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.enc != nil {
		w.enc.SendFrame(nil)
		w.drain(w.enc, w.video)
		w.enc.Free()
	}
	if w.audioCC != nil {
		w.audioCC.SendFrame(nil)
		w.drain(w.audioCC, w.audio)
		w.audioCC.Free()
	}
	var err error
	if w.headerWritten {
		if err := w.fc.WriteTrailer(); err != nil {
			err = fmt.Errorf("fixture: write trailer: %w", err)
		}
	}
	w.io.Close()
	w.fc.Free()
	return err
}

func isEOF(err error) bool { return errors.Is(err, astiav.ErrEof) }

// yuv420Frame renders a deterministic test pattern.
func yuv420Frame(width, height, index int) []byte {
	y := make([]byte, width*height)
	u := make([]byte, width*height/4)
	v := make([]byte, width*height/4)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for py := 0; py < height; py++ {
		for px := 0; px < width; px++ {
			img.Set(px, py, color.RGBA{
				R: uint8((px*2 + index*7) % 256),
				G: uint8((py*3 + index*5) % 256),
				B: uint8((index * 11) % 256),
				A: 255,
			})
		}
	}
	for py := 0; py < height; py++ {
		for px := 0; px < width; px++ {
			r, g, b, _ := img.At(px, py).RGBA()
			yy := 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			y[py*width+px] = uint8(clamp(yy, 16, 235))
		}
	}
	for py := 0; py < height/2; py++ {
		for px := 0; px < width/2; px++ {
			r, g, b, _ := img.At(px*2, py*2).RGBA()
			cb := -0.169*float64(r>>8) - 0.331*float64(g>>8) + 0.5*float64(b>>8) + 128
			cr := 0.5*float64(r>>8) - 0.419*float64(g>>8) - 0.081*float64(b>>8) + 128
			u[py*(width/2)+px] = uint8(clamp(cb, 16, 240))
			v[py*(width/2)+px] = uint8(clamp(cr, 16, 240))
		}
	}
	return append(append(y, u...), v...)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// audioSamples renders planar float32 stereo (the layout astiav fltp frames
// expect: all of channel 0, then all of channel 1).
func audioSamples(rate, frames int, startFrame int, hz float64, amp float64) []byte {
	out := make([]byte, frames*2*4)
	for i := 0; i < frames; i++ {
		t := float64(startFrame+i) / float64(rate)
		l := amp * math.Sin(2*math.Pi*hz*t)
		r := amp * 0.8 * math.Sin(2*math.Pi*hz*1.5*t)
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(float32(l)))
		binary.LittleEndian.PutUint32(out[(frames+i)*4:], math.Float32bits(float32(r)))
	}
	return out
}
