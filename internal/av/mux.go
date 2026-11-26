package av

import (
	"errors"
	"math"
	"os"
	"path/filepath"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// Encoder streams interleaved stereo float32 into a temporary AAC/MP4 file
// and only publishes it after a successful trailer. No data is written to the
// final path before Verify succeeds.
type Encoder struct {
	path      string
	tmpPath   string
	ofc       *astiav.FormatContext
	codec     *astiav.Codec
	cc        *astiav.CodecContext
	stream    *astiav.Stream
	io        *astiav.IOContext
	pkt       *astiav.Packet
	rate      int
	pending   []float32
	frames    int64
	payload   int64
	closed    bool
	finalized bool
}

// NewEncoderAAC prepares an AAC encoder writing to a temp file beside path.
func NewEncoderAAC(path string, rate int, kbps int) (*Encoder, error) {
	if rate <= 0 {
		rate = OutputRate
	}
	if kbps <= 0 {
		kbps = TargetKbps
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.NewError(media.ErrIO, "creating output directory: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".mediad-encode-*.tmp")
	if err != nil {
		return nil, media.NewError(media.ErrIO, "creating temp output: %v", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrIO, "closing temp output: %v", err)
	}
	// The muxer infers the container from the extension; keep .m4a.
	containerPath := tmpName + ".m4a"
	os.Remove(tmpName)
	tmpName = containerPath

	ofc, err := astiav.AllocOutputFormatContext(nil, "mp4", tmpName)
	if err != nil || ofc == nil {
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "allocating output context: %v", err)
	}
	ofc.SetFlags(ofc.Flags().Add(astiav.FormatContextFlagBitexact))

	codec := astiav.FindEncoderByName("aac")
	if codec == nil {
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrUnsupported, "AAC encoder unavailable")
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "cannot allocate AAC context")
	}
	cc.SetSampleRate(rate)
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	cc.SetChannelLayout(astiav.ChannelLayoutStereo)
	cc.SetTimeBase(astiav.NewRational(1, rate))
	cc.SetBitRate(int64(kbps) * 1000)
	cc.SetThreadCount(1)
	cc.SetFlags(cc.Flags().Add(astiav.CodecContextFlagBitexact))
	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "opening AAC encoder: %v", err)
	}

	stream := ofc.NewStream(nil)
	if stream == nil {
		cc.Free()
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "cannot create output stream")
	}
	if err := stream.CodecParameters().FromCodecContext(cc); err != nil {
		cc.Free()
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "copying encoder parameters: %v", err)
	}
	stream.SetTimeBase(astiav.NewRational(1, rate))

	io, err := astiav.OpenIOContext(tmpName, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		cc.Free()
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrIO, "opening temp output: %v", err)
	}
	ofc.SetPb(io)
	if err := ofc.WriteHeader(nil); err != nil {
		io.Close()
		io.Free()
		cc.Free()
		ofc.Free()
		os.Remove(tmpName)
		return nil, media.NewError(media.ErrEncode, "writing header: %v", err)
	}

	e := &Encoder{
		path: path, tmpPath: tmpName, ofc: ofc, codec: codec, cc: cc,
		stream: stream, io: io, pkt: astiav.AllocPacket(), rate: rate,
	}
	if e.pkt == nil {
		e.Abort()
		return nil, media.NewError(media.ErrEncode, "cannot allocate packet")
	}
	return e, nil
}

// TempPath exposes the in-progress file for checkpoint bookkeeping.
func (e *Encoder) TempPath() string { return e.tmpPath }

// WriteSamples appends interleaved stereo float32 samples, buffering partial
// AAC frames.
func (e *Encoder) WriteSamples(samples []float32) error {
	if e.closed {
		return media.NewError(media.ErrEncode, "encoder already closed")
	}
	e.pending = append(e.pending, samples...)
	const frameSize = 1024
	for len(e.pending) >= frameSize*2 {
		chunk := e.pending[:frameSize*2]
		if err := e.encodeChunk(chunk); err != nil {
			return err
		}
		e.pending = e.pending[frameSize*2:]
	}
	return nil
}

func (e *Encoder) encodeChunk(interleaved []float32) error {
	n := len(interleaved) / 2
	f := astiav.AllocFrame()
	if f == nil {
		return media.NewError(media.ErrEncode, "cannot allocate frame")
	}
	defer f.Free()
	f.SetSampleRate(e.rate)
	f.SetSampleFormat(astiav.SampleFormatFltp)
	f.SetChannelLayout(astiav.ChannelLayoutStereo)
	f.SetNbSamples(n)
	f.SetPts(e.frames)
	if err := f.AllocBuffer(1); err != nil {
		return media.NewError(media.ErrEncode, "allocating encode frame: %v", err)
	}
	buf := make([]byte, n*2*4)
	for c := 0; c < 2; c++ {
		for i := 0; i < n; i++ {
			bits := float32bits(interleaved[i*2+c])
			off := c*n*4 + i*4
			buf[off] = byte(bits)
			buf[off+1] = byte(bits >> 8)
			buf[off+2] = byte(bits >> 16)
			buf[off+3] = byte(bits >> 24)
		}
	}
	if err := f.Data().SetBytes(buf, 1); err != nil {
		return media.NewError(media.ErrEncode, "filling encode frame: %v", err)
	}
	if err := e.cc.SendFrame(f); err != nil {
		return media.NewError(media.ErrEncode, "sending frame: %v", err)
	}
	e.frames += int64(n)
	return e.drain()
}

func (e *Encoder) drain() error {
	for {
		if err := e.cc.ReceivePacket(e.pkt); err != nil {
			if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
				return nil
			}
			return media.NewError(media.ErrEncode, "receiving packet: %v", err)
		}
		e.payload += int64(e.pkt.Size())
		e.pkt.SetStreamIndex(e.stream.Index())
		e.pkt.RescaleTs(e.cc.TimeBase(), e.stream.TimeBase())
		if err := e.ofc.WriteInterleavedFrame(e.pkt); err != nil {
			e.pkt.Unref()
			return media.NewError(media.ErrEncode, "writing packet: %v", err)
		}
		e.pkt.Unref()
	}
}

// Close flushes the encoder and writes the trailer. The temp file is complete
// but not yet published; call Commit to move it into place.
func (e *Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if len(e.pending) > 0 {
		// Pad the final frame with silence so the last samples are encoded.
		pad := make([]float32, (1024*2)-len(e.pending))
		e.pending = append(e.pending, pad...)
		if err := e.encodeChunk(e.pending); err != nil {
			return err
		}
		e.pending = nil
	}
	if err := e.cc.SendFrame(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return media.NewError(media.ErrEncode, "flushing encoder: %v", err)
	}
	if err := e.drain(); err != nil {
		return err
	}
	if err := e.ofc.WriteTrailer(); err != nil {
		return media.NewError(media.ErrEncode, "writing trailer: %v", err)
	}
	if err := e.io.Close(); err != nil {
		return media.NewError(media.ErrIO, "closing temp output: %v", err)
	}
	e.finalized = true
	return nil
}

// PayloadBytes reports the encoded audio payload written so far.
func (e *Encoder) PayloadBytes() int64 { return e.payload }

// SamplesFed reports how many source samples were encoded.
func (e *Encoder) SamplesFed() int64 { return e.frames }

// Commit atomically moves the verified temp file to the final path.
func (e *Encoder) Commit() error {
	if !e.finalized {
		return media.NewError(media.ErrEncode, "cannot commit before close")
	}
	if err := os.Rename(e.tmpPath, e.path); err != nil {
		return media.NewError(media.ErrIO, "publishing output: %v", err)
	}
	e.Release()
	return nil
}

// Abort discards the temp file and frees resources. It never touches the
// final output path.
func (e *Encoder) Abort() {
	if e.io != nil {
		_ = e.io.Close()
		e.io.Free()
		e.io = nil
	}
	if e.pkt != nil {
		e.pkt.Free()
		e.pkt = nil
	}
	if e.cc != nil {
		e.cc.Free()
		e.cc = nil
	}
	if e.ofc != nil {
		e.ofc.Free()
		e.ofc = nil
	}
	if e.tmpPath != "" {
		os.Remove(e.tmpPath)
		e.tmpPath = ""
	}
}

// Release frees resources without deleting an already-published file.
func (e *Encoder) Release() {
	if e.io != nil {
		e.io.Free()
		e.io = nil
	}
	if e.pkt != nil {
		e.pkt.Free()
		e.pkt = nil
	}
	if e.cc != nil {
		e.cc.Free()
		e.cc = nil
	}
	if e.ofc != nil {
		e.ofc.Free()
		e.ofc = nil
	}
	e.tmpPath = ""
}

func float32bits(v float32) uint32 { return math.Float32bits(v) }

// DecodeMonoPacked is a small helper for fixture comparisons.
func DecodeMonoPacked(path string) ([]float32, int, error) {
	fc, err := openInput(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()
	var stream *astiav.Stream
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			stream = s
			break
		}
	}
	if stream == nil {
		return nil, 0, media.NewError(media.ErrDemux, "no audio stream")
	}
	cp := stream.CodecParameters()
	decoder := astiav.FindDecoder(cp.CodecID())
	dec := astiav.AllocCodecContext(decoder)
	defer dec.Free()
	if err := cp.ToCodecContext(dec); err != nil {
		return nil, 0, media.NewError(media.ErrDemux, "codec parameters: %v", err)
	}
	dec.SetTimeBase(stream.TimeBase())
	if err := dec.Open(decoder, nil); err != nil {
		return nil, 0, media.NewError(media.ErrDemux, "opening decoder: %v", err)
	}
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()
	var out []float32
	rate := cp.SampleRate()
	for {
		if err := fc.ReadFrame(pkt); err != nil {
			break
		}
		if pkt.StreamIndex() != stream.Index() {
			pkt.Unref()
			continue
		}
		_ = dec.SendPacket(pkt)
		pkt.Unref()
		for {
			if err := dec.ReceiveFrame(frame); err != nil {
				break
			}
			raw, _ := frame.Data().Bytes(1)
			ch := frame.ChannelLayout().Channels()
			n := frame.NbSamples()
			for i := 0; i < n; i++ {
				var sum float32
				for c := 0; c < ch; c++ {
					off := c*n*4 + i*4
					if off+4 > len(raw) {
						continue
					}
					bits := uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16 | uint32(raw[off+3])<<24
					sum += math.Float32frombits(bits)
				}
				out = append(out, sum/float32(ch))
			}
			frame.Unref()
		}
	}
	return out, rate, nil
}
