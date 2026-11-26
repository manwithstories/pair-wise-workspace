package av

import (
	"errors"
	"math"
	"math/big"

	"github.com/asticode/go-astiav"

	"mediad/internal/media"
)

// Delivery constants: 48 kHz stereo AAC at 192 kbps.
const (
	OutputRate     = 48000
	OutputChannels = 2
	TargetKbps     = 192
)

// MaxSegmentSamples bounds the in-memory queue for one edit: 5 minutes of
// 48 kHz stereo float. Longer edits are rejected rather than buffered without
// bound.
const MaxSegmentSamples = OutputRate * 300

// MixMatrix maps an N-channel source onto the stereo delivery bus. Each of the
// two rows holds one coefficient per source channel, in source channel order.
type MixMatrix [2][]float64

// StereoPassthrough is the explicit identity matrix for stereo sources.
func StereoPassthrough() MixMatrix { return MixMatrix{{1, 0}, {0, 1}} }

// MonoCentered duplicates a mono source into both delivery channels.
func MonoCentered() MixMatrix { return MixMatrix{{1}, {1}} }

// DefaultMatrix returns the explicit matrix for a known layout. Unknown
// layouts are rejected rather than guessed.
func DefaultMatrix(layout string, channels int) (MixMatrix, error) {
	switch channels {
	case 1:
		return MonoCentered(), nil
	case 2:
		return StereoPassthrough(), nil
	}
	switch layout {
	case "5.1", "5.1(side)":
		if channels == 6 {
			return MixMatrix{
				{1.0, 0.0, 0.707, 0.0, 0.707, 0.0},
				{0.0, 1.0, 0.707, 0.0, 0.0, 0.707},
			}, nil
		}
	case "quad":
		if channels == 4 {
			return MixMatrix{
				{1.0, 0.0, 0.707, 0.0},
				{0.0, 1.0, 0.0, 0.707},
			}, nil
		}
	}
	return MixMatrix{}, media.NewError(media.ErrUnsupported, "layout %q with %d channels has no explicit mixing matrix", layout, channels)
}

// ValidateMatrix checks the matrix shape and coefficient range.
func ValidateMatrix(m MixMatrix, channels int) error {
	if len(m) != 2 || len(m[0]) == 0 || len(m[1]) == 0 {
		return media.NewError(media.ErrUnsupported, "mix matrix must have two non-empty rows")
	}
	for r := 0; r < 2; r++ {
		if channels > 0 && len(m[r]) != channels {
			return media.NewError(media.ErrUnsupported, "matrix row %d has %d coefficients for %d channels", r, len(m[r]), channels)
		}
		for _, v := range m[r] {
			if math.IsNaN(v) || math.Abs(v) > 8 {
				return media.NewError(media.ErrUnsupported, "matrix coefficient %.3f out of range", v)
			}
		}
	}
	return nil
}

// SegmentRequest selects a half-open range of one source track, expressed in
// the track's raw ticks, and asks for it as stereo float at 48 kHz.
type SegmentRequest struct {
	Path        string
	StreamIndex int
	TimeBase    media.Rational
	InRaw       int64
	OutRaw      int64
	InLayout    string
	InChannels  int
	Matrix      MixMatrix
}

// Damage records one damaged source interval that was replaced with silence.
type Damage struct {
	StreamIndex int    `json:"stream_index"`
	StartRaw    int64  `json:"start_raw"`
	EndRaw      int64  `json:"end_raw"`
	Reason      string `json:"reason"`
	Recovered   bool   `json:"recovered"`
}

// SegResult is the mixed stereo segment plus recovery diagnostics.
type SegResult struct {
	Samples       []float32
	LengthSamples int
	Damaged       []Damage
	DecodedFrames int
	// AlignmentUS is the round-off between the manifest cut points and the
	// nearest 48 kHz sample; the contract is at most one output sample (20.8 µs).
	AlignmentUS float64
	// SourceTicks is the raw span actually consumed.
	SourceTicks int64
}

// SignedSpan converts a raw tick delta to samples at the given rate, rounded
// to nearest and allowed to be negative.
func SignedSpan(tb media.Rational, from, to int64, rate int) (int64, error) {
	if tb.Num <= 0 || tb.Den <= 0 {
		return 0, media.NewError(media.ErrInvalidTimebase, "invalid time base %d/%d", tb.Num, tb.Den)
	}
	if rate <= 0 {
		return 0, media.NewError(media.ErrInvalidTimebase, "invalid sample rate %d", rate)
	}
	num := big.NewInt(to - from)
	num.Mul(num, big.NewInt(tb.Num))
	num.Mul(num, big.NewInt(int64(rate)))
	den := big.NewInt(tb.Den)
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}
	half := new(big.Int).Rsh(new(big.Int).Set(den), 1)
	q := new(big.Int).Add(num, half)
	q.Quo(q, den)
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, media.NewError(media.ErrUnsupported, "span %d..%d overflows", from, to)
	}
	return q.Int64(), nil
}

// SpanSamples is SignedSpan restricted to non-negative ranges at 48 kHz.
func SpanSamples(tb media.Rational, from, to int64) (int, error) {
	if from > to {
		return 0, media.NewError(media.ErrManifest, "negative span %d -> %d", from, to)
	}
	v, err := SignedSpan(tb, from, to, OutputRate)
	if err != nil {
		return 0, err
	}
	if v > math.MaxInt32 {
		return 0, media.NewError(media.ErrUnsupported, "span %d..%d too long", from, to)
	}
	return int(v), nil
}

// SpanAlignmentUS reports the sub-sample rounding error of SpanSamples.
func SpanAlignmentUS(tb media.Rational, from, to int64) (float64, error) {
	return spanAlignmentUS(tb, from, to, OutputRate)
}

func spanAlignmentUS(tb media.Rational, from, to int64, rate int) (float64, error) {
	if tb.Num <= 0 || tb.Den <= 0 {
		return 0, media.NewError(media.ErrInvalidTimebase, "invalid time base %d/%d", tb.Num, tb.Den)
	}
	num := big.NewInt(to - from)
	num.Mul(num, big.NewInt(tb.Num))
	num.Mul(num, big.NewInt(int64(rate)))
	den := big.NewInt(tb.Den)
	rem := new(big.Int).Rem(num, den)
	frac, _ := new(big.Float).Quo(new(big.Float).SetInt(rem), new(big.Float).SetInt(den)).Float64()
	frac = math.Abs(frac)
	if frac > 0.5 {
		frac = 1 - frac
	}
	return frac * 1e6 / float64(rate), nil
}

// ReadSegment decodes, resamples and mixes one edit range into interleaved
// stereo float32. The returned slice is always exactly LengthSamples*2 long.
// Damaged input is replaced with silence and reported; it never aborts the
// delivery.
func ReadSegment(req SegmentRequest) (*SegResult, error) {
	if req.InChannels == 0 {
		return nil, media.NewError(media.ErrUnsupported, "source channel count unknown")
	}
	matrix := req.Matrix
	if matrix[0] == nil || matrix[1] == nil {
		m, err := DefaultMatrix(req.InLayout, req.InChannels)
		if err != nil {
			return nil, err
		}
		matrix = m
	}
	if err := ValidateMatrix(matrix, req.InChannels); err != nil {
		return nil, err
	}
	if req.InRaw >= req.OutRaw {
		return nil, media.NewError(media.ErrManifest, "segment range must be positive (%d -> %d)", req.InRaw, req.OutRaw)
	}
	length, err := SpanSamples(req.TimeBase, req.InRaw, req.OutRaw)
	if err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, media.NewError(media.ErrManifest, "segment has zero length")
	}
	if length > MaxSegmentSamples {
		return nil, media.NewError(media.ErrUnsupported, "segment of %d samples exceeds the bounded queue", length)
	}
	alignUS, err := SpanAlignmentUS(req.TimeBase, req.InRaw, req.OutRaw)
	if err != nil {
		return nil, err
	}

	decoded, err := decodeRange(req)
	if err != nil {
		return nil, err
	}

	res := &SegResult{LengthSamples: length, AlignmentUS: alignUS, Damaged: decoded.damaged, DecodedFrames: decoded.frames, SourceTicks: req.OutRaw - req.InRaw}
	stereo, err := resampleAndMix(decoded, req, matrix, length)
	if err != nil {
		return nil, err
	}
	res.Samples = stereo
	return res, nil
}

// decodedRange is the source-rate planar audio covering the edit range.
type decodedRange struct {
	rate     int
	channels int
	layout   astiav.ChannelLayout
	planes   [][]float32 // each exactly `length` samples, zero-filled gaps
	length   int
	frames   int
	damaged  []Damage
	// addressable says whether any real audio landed in the range.
	addressable bool
}

// decodeRange decodes the overlapping frames and places them at sample offsets
// relative to InRaw, so the cut is sample-accurate for any time base.
func decodeRange(req SegmentRequest) (*decodedRange, error) {
	fc, err := openInput(req.Path)
	if err != nil {
		return nil, err
	}
	defer func() {
		fc.CloseInput()
		fc.Free()
	}()

	var stream *astiav.Stream
	for _, s := range fc.Streams() {
		if s.Index() == req.StreamIndex {
			stream = s
			break
		}
	}
	if stream == nil {
		return nil, media.NewError(media.ErrDemux, "stream %d missing from %s", req.StreamIndex, req.Path)
	}
	cp := stream.CodecParameters()
	decoder := astiav.FindDecoder(cp.CodecID())
	if decoder == nil {
		return nil, media.NewError(media.ErrUnsupported, "no decoder for %s", cp.CodecID().Name())
	}
	dec := astiav.AllocCodecContext(decoder)
	if dec == nil {
		return nil, media.NewError(media.ErrDemux, "cannot allocate decoder")
	}
	defer dec.Free()
	if err := cp.ToCodecContext(dec); err != nil {
		return nil, media.NewError(media.ErrDemux, "copying codec parameters: %v", err)
	}
	dec.SetTimeBase(stream.TimeBase())
	if err := dec.Open(decoder, nil); err != nil {
		return nil, media.NewError(media.ErrDemux, "opening decoder: %v", err)
	}

	preroll := stream.TimeBase().Den() / 2
	seekTarget := req.InRaw - int64(preroll)
	if seekTarget < 0 {
		seekTarget = 0
	}
	_ = fc.SeekFrame(req.StreamIndex, seekTarget, astiav.NewSeekFlags().Add(astiav.SeekFlagBackward))

	out := &decodedRange{}
	var lastPktPts int64 = math.MinInt64
	var pendingDamage int64 = -1
	var pendingReason string
	var lastFrameEnd int64 = math.MinInt64
	var lastDamagedEnd int64 = math.MinInt64
	var readErr error

	// ensure allocates the placement buffer once the format is known.
	ensure := func(rate, channels int, layout astiav.ChannelLayout) error {
		if out.rate == 0 {
			out.rate = rate
			out.channels = channels
			out.layout = layout
			l, err := SignedSpan(req.TimeBase, req.InRaw, req.OutRaw, rate)
			if err != nil {
				return err
			}
			if l <= 0 || l > int64(MaxSegmentSamples*8) {
				return media.NewError(media.ErrUnsupported, "segment length %d samples at %d Hz out of bounds", l, rate)
			}
			out.length = int(l)
			out.planes = make([][]float32, channels)
			for c := range out.planes {
				out.planes[c] = make([]float32, out.length)
			}
		} else if rate != out.rate || channels != out.channels {
			return media.NewError(media.ErrUnsupported, "mid-stream audio format change at %d", lastPktPts)
		}
		return nil
	}

	place := func(f *astiav.Frame) error {
		if f.NbSamples() <= 0 {
			return nil
		}
		ch := f.ChannelLayout().Channels()
		rate := f.SampleRate()
		if rate <= 0 {
			rate = cp.SampleRate()
		}
		if ch <= 0 || rate <= 0 {
			return media.NewError(media.ErrUnsupported, "decoded frame lacks rate or layout")
		}
		// Normalise the decoded sample format to packed float32 before placing,
		// so integer and planar sources share one code path.
		planar, err := frameToPlanarFloat(f, ch)
		if err != nil {
			return err
		}
		if err := ensure(rate, ch, f.ChannelLayout()); err != nil {
			return err
		}
		if out.channels != ch {
			return media.NewError(media.ErrUnsupported, "decoded channel count %d disagrees with %d", ch, out.channels)
		}
		n := f.NbSamples()
		start := 0
		if f.Pts() != astiav.NoPtsValue {
			rel, err := SignedSpan(req.TimeBase, req.InRaw, f.Pts(), rate)
			if err != nil {
				return err
			}
			start = int(rel)
		}
		for i := 0; i < n; i++ {
			pos := start + i
			if pos < 0 || pos >= out.length {
				continue
			}
			for c := 0; c < ch; c++ {
				out.planes[c][pos] = planar[c][i]
			}
		}
		out.frames++
		out.addressable = true
		if f.Pts() != astiav.NoPtsValue {
			end := f.Pts() + int64(math.Ceil(float64(n)*float64(req.TimeBase.Den)/float64(req.TimeBase.Num)/float64(rate)))
			if end > lastFrameEnd {
				lastFrameEnd = end
			}
		}
		return nil
	}

	flushPending := func(endRaw int64) {
		if pendingDamage < 0 {
			return
		}
		if endRaw <= req.InRaw {
			pendingDamage = -1
			return
		}
		start := pendingDamage
		if start < req.InRaw {
			start = req.InRaw
		}
		if endRaw > req.OutRaw {
			endRaw = req.OutRaw
		}
		if endRaw > start {
			out.damaged = append(out.damaged, Damage{StreamIndex: req.StreamIndex, StartRaw: start, EndRaw: endRaw, Reason: pendingReason, Recovered: true})
		}
		pendingDamage = -1
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()

	drain := func() error {
		for {
			err := dec.ReceiveFrame(frame)
			if err != nil {
				if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
					return nil
				}
				if pendingDamage < 0 && lastPktPts != math.MinInt64 {
					pendingDamage = lastPktPts
					pendingReason = "undecodable packet: " + err.Error()
				}
				return nil
			}
			if frame.Pts() != astiav.NoPtsValue {
				flushPending(frame.Pts())
			}
			if err := place(frame); err != nil {
				frame.Unref()
				return err
			}
			frame.Unref()
		}
	}

	for {
		if err := fc.ReadFrame(pkt); err != nil {
			if !errors.Is(err, astiav.ErrEof) {
				readErr = err
			}
			break
		}
		if pkt.StreamIndex() != req.StreamIndex {
			pkt.Unref()
			continue
		}
		pts := pkt.Pts()
		if pts != astiav.NoPtsValue {
			if pkt.Duration() > 0 && pts+pkt.Duration() <= req.InRaw {
				pkt.Unref()
				continue
			}
			if pts >= req.OutRaw {
				pkt.Unref()
				break
			}
			lastPktPts = pts
		}
		if len(pkt.Data()) == 0 {
			if pendingDamage < 0 && pts != astiav.NoPtsValue {
				pendingDamage = pts
				pendingReason = "empty packet"
			}
			pkt.Unref()
			continue
		}
		// A packet the demuxer flags corrupt is short or damaged. Record the
		// track and time, skip its payload and let the damage window extend
		// until the next clean frame arrives.
		if pkt.Flags().Has(astiav.PacketFlagCorrupt) {
			if pendingDamage < 0 {
				pendingDamage = pts
				pendingReason = "corrupt packet (truncated payload)"
				if pendingDamage < 0 {
					pendingDamage = req.InRaw
				}
			}
			lastDamagedEnd = pts
			if pkt.Duration() > 0 {
				lastDamagedEnd = pts + pkt.Duration()
			}
			pkt.Unref()
			continue
		}
		if err := dec.SendPacket(pkt); err != nil && !errors.Is(err, astiav.ErrEagain) {
			if pendingDamage < 0 && pts != astiav.NoPtsValue {
				pendingDamage = pts
				pendingReason = "undecodable packet: " + err.Error()
			}
		}
		pkt.Unref()
		if err := drain(); err != nil {
			return out, err
		}
	}
	_ = dec.SendPacket(nil)
	if err := drain(); err != nil {
		return out, err
	}
	// Any damage still open at EOF extends to the end of the requested range,
	// using the last damaged packet's end when it is known.
	if pendingDamage >= 0 {
		end := req.OutRaw
		if lastDamagedEnd > req.InRaw && lastDamagedEnd < end {
			end = lastDamagedEnd
		}
		flushPending(end)
	}
	flushPending(req.OutRaw)

	if readErr != nil {
		start := lastFrameEnd
		if start < req.InRaw {
			start = req.InRaw
		}
		if start < req.OutRaw {
			out.damaged = append(out.damaged, Damage{StreamIndex: req.StreamIndex, StartRaw: start, EndRaw: req.OutRaw, Reason: "read error: " + readErr.Error(), Recovered: true})
		}
	}
	return out, nil
}

// resampleAndMix converts the source-rate range to 48 kHz stereo and applies
// the explicit matrix. The result is trimmed or padded to exactly length.
func resampleAndMix(src *decodedRange, req SegmentRequest, matrix MixMatrix, length int) ([]float32, error) {
	out := make([]float32, length*OutputChannels)
	if src == nil || src.rate == 0 || src.length == 0 {
		return out, nil
	}

	// Fast path: already 48 kHz float planar. Only the matrix is applied.
	if src.rate == OutputRate {
		for i := 0; i < src.length && i < length; i++ {
			var l, r float64
			for c := 0; c < src.channels && c < len(matrix[0]); c++ {
				v := float64(src.planes[c][i])
				l += matrix[0][c] * v
				r += matrix[1][c] * v
			}
			out[i*2] = float32(l)
			out[i*2+1] = float32(r)
		}
		return out, nil
	}

	swr := astiav.AllocSoftwareResampleContext()
	if swr == nil {
		return nil, media.NewError(media.ErrEncode, "cannot allocate resampler")
	}
	defer swr.Free()

	// Output is stereo at 48 kHz so the resampler handles the channel fold as
	// part of the conversion; the explicit matrix is applied afterwards via a
	// temporary N-channel conversion. To keep one code path we resample to the
	// source layout and mix in Go.
	pos := 0
	wrote := 0
	const chunk = 8192
	for pos < src.length {
		n := chunk
		if pos+n > src.length {
			n = src.length - pos
		}
		in := astiav.AllocFrame()
		in.SetSampleRate(src.rate)
		in.SetSampleFormat(astiav.SampleFormatFltp)
		in.SetChannelLayout(src.layout)
		in.SetNbSamples(n)
		if err := in.AllocBuffer(1); err != nil {
			in.Free()
			return nil, media.NewError(media.ErrEncode, "allocating resample input: %v", err)
		}
		buf := make([]byte, n*src.channels*4)
		for c := 0; c < src.channels; c++ {
			for i := 0; i < n; i++ {
				bits := math.Float32bits(src.planes[c][pos+i])
				off := c*n*4 + i*4
				buf[off] = byte(bits)
				buf[off+1] = byte(bits >> 8)
				buf[off+2] = byte(bits >> 16)
				buf[off+3] = byte(bits >> 24)
			}
		}
		if err := in.Data().SetBytes(buf, 1); err != nil {
			in.Free()
			return nil, media.NewError(media.ErrEncode, "filling resample input: %v", err)
		}
		dst := astiav.AllocFrame()
		dst.SetSampleRate(OutputRate)
		dst.SetSampleFormat(astiav.SampleFormatFltp)
		dst.SetChannelLayout(src.layout)
		dst.SetNbSamples(n*OutputRate/src.rate + 256)
		if err := dst.AllocBuffer(1); err != nil {
			in.Free()
			dst.Free()
			return nil, media.NewError(media.ErrEncode, "allocating resample output: %v", err)
		}
		if err := swr.ConvertFrame(in, dst); err != nil {
			in.Free()
			dst.Free()
			return nil, media.NewError(media.ErrEncode, "resampling audio: %v", err)
		}
		in.Free()
		if err := mixFrameInto(out, dst, src.channels, matrix, length, &wrote); err != nil {
			dst.Free()
			return nil, err
		}
		dst.Free()
		pos += n
	}
	// Flush the resampler delay.
	for {
		dst := astiav.AllocFrame()
		dst.SetSampleRate(OutputRate)
		dst.SetSampleFormat(astiav.SampleFormatFltp)
		dst.SetChannelLayout(src.layout)
		dst.SetNbSamples(1024)
		if err := dst.AllocBuffer(1); err != nil {
			dst.Free()
			break
		}
		if err := swr.ConvertFrame(nil, dst); err != nil {
			dst.Free()
			break
		}
		n := dst.NbSamples()
		if n == 0 {
			dst.Free()
			break
		}
		if err := mixFrameInto(out, dst, src.channels, matrix, length, &wrote); err != nil {
			dst.Free()
			return nil, err
		}
		dst.Free()
		if swr.Delay(int64(OutputRate)) <= 0 {
			break
		}
	}
	return out, nil
}

// mixFrameInto appends a resampled frame's samples into the interleaved stereo
// output, applying the matrix. wrote is the running output sample cursor.
func mixFrameInto(out []float32, f *astiav.Frame, channels int, matrix MixMatrix, length int, wrote *int) error {
	n := f.NbSamples()
	if n <= 0 {
		return nil
	}
	raw, err := f.Data().Bytes(1)
	if err != nil {
		return media.NewError(media.ErrEncode, "reading resampled frame: %v", err)
	}
	ch := f.ChannelLayout().Channels()
	if ch <= 0 {
		ch = channels
	}
	if len(raw) < n*ch*4 {
		return media.NewError(media.ErrEncode, "resampled buffer too small")
	}
	for i := 0; i < n; i++ {
		pos := *wrote + i
		if pos >= length {
			break
		}
		var l, r float64
		for c := 0; c < ch && c < len(matrix[0]); c++ {
			off := c*n*4 + i*4
			bits := uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16 | uint32(raw[off+3])<<24
			v := float64(math.Float32frombits(bits))
			l += matrix[0][c] * v
			r += matrix[1][c] * v
		}
		out[pos*2] = float32(l)
		out[pos*2+1] = float32(r)
	}
	*wrote += n
	if *wrote > length {
		*wrote = length
	}
	return nil
}

// frameToPlanarFloat converts any decoded audio frame's sample format to
// planar float32 in place (channel-major slices), covering the integer and
// packed formats the pipeline can encounter.
func frameToPlanarFloat(f *astiav.Frame, channels int) ([][]float32, error) {
	n := f.NbSamples()
	sf := f.SampleFormat()
	out := make([][]float32, channels)
	raw, err := f.Data().Bytes(1)
	if err != nil {
		return nil, media.NewError(media.ErrCorrupt, "reading decoded frame: %v", err)
	}
	switch sf {
	case astiav.SampleFormatFltp:
		if len(raw) < n*channels*4 {
			return nil, media.NewError(media.ErrCorrupt, "planar float buffer too small")
		}
		for c := 0; c < channels; c++ {
			plane := make([]float32, n)
			for i := 0; i < n; i++ {
				off := c*n*4 + i*4
				plane[i] = math.Float32frombits(uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16 | uint32(raw[off+3])<<24)
			}
			out[c] = plane
		}
	case astiav.SampleFormatFlt:
		if len(raw) < n*channels*4 {
			return nil, media.NewError(media.ErrCorrupt, "packed float buffer too small")
		}
		for c := 0; c < channels; c++ {
			out[c] = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			for c := 0; c < channels; c++ {
				off := (i*channels + c) * 4
				out[c][i] = math.Float32frombits(uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16 | uint32(raw[off+3])<<24)
			}
		}
	case astiav.SampleFormatS16:
		if len(raw) < n*channels*2 {
			return nil, media.NewError(media.ErrCorrupt, "s16 buffer too small")
		}
		for c := 0; c < channels; c++ {
			out[c] = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			for c := 0; c < channels; c++ {
				off := (i*channels + c) * 2
				v := int16(uint16(raw[off]) | uint16(raw[off+1])<<8)
				out[c][i] = float32(v) / 32768.0
			}
		}
	case astiav.SampleFormatS16P:
		if len(raw) < n*channels*2 {
			return nil, media.NewError(media.ErrCorrupt, "s16p buffer too small")
		}
		for c := 0; c < channels; c++ {
			plane := make([]float32, n)
			for i := 0; i < n; i++ {
				off := c*n*2 + i*2
				v := int16(uint16(raw[off]) | uint16(raw[off+1])<<8)
				plane[i] = float32(v) / 32768.0
			}
			out[c] = plane
		}
	case astiav.SampleFormatS32, astiav.SampleFormatS32P:
		if len(raw) < n*channels*4 {
			return nil, media.NewError(media.ErrCorrupt, "s32 buffer too small")
		}
		for c := 0; c < channels; c++ {
			out[c] = make([]float32, n)
		}
		for i := 0; i < n; i++ {
			for c := 0; c < channels; c++ {
				off := 0
				if sf == astiav.SampleFormatS32P {
					off = c*n*4 + i*4
				} else {
					off = (i*channels + c) * 4
				}
				v := int32(uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16 | uint32(raw[off+3])<<24)
				out[c][i] = float32(float64(v) / 2147483648.0)
			}
		}
	default:
		return nil, media.NewError(media.ErrUnsupported, "sample format %s not convertible", sf.Name())
	}
	return out, nil
}
