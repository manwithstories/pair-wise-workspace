// Package opus wraps the libopus C API for fixture generation and for
// recovery: encoding real Opus packets, decoding them for stateful PLC and
// in-band FEC, and building the OpusHead extradata Matroska needs.
//
// Only local, self-generated audio is processed.  The wrapper owns every C
// allocation it creates and exposes explicit Close methods so a canceled run
// does not leak encoder or decoder state.
package opus

/*
#cgo pkg-config: opus
#include <opus.h>
#include <string.h>

static int mc_opus_encoder_new(int rate, int channels, int application, OpusEncoder **out) {
	int err = OPUS_OK;
	OpusEncoder *e = opus_encoder_create(rate, channels, application, &err);
	if (err != OPUS_OK) {
		*out = NULL;
		return err;
	}
	*out = e;
	return OPUS_OK;
}

// The OPUS_SET_* helpers are function-like macros and cannot be referenced
// through cgo, so mirror the four we use as plain functions.
static int mc_opus_set_bitrate(OpusEncoder *e, int v) { return opus_encoder_ctl(e, OPUS_SET_BITRATE(v)); }
static int mc_opus_set_inband_fec(OpusEncoder *e, int v) { return opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(v)); }
static int mc_opus_set_packet_loss(OpusEncoder *e, int v) { return opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(v)); }
static int mc_opus_set_complexity(OpusEncoder *e, int v) { return opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(v)); }

static int mc_opus_ctl(OpusEncoder *e, int which, int v) {
	switch (which) {
	case 0: return mc_opus_set_bitrate(e, v);
	case 1: return mc_opus_set_inband_fec(e, v);
	case 2: return mc_opus_set_packet_loss(e, v);
	case 3: return mc_opus_set_complexity(e, v);
	default: return OPUS_BAD_ARG;
	}
}

static int mc_opus_lookahead(OpusEncoder *e) {
	int v = 0;
	int err = opus_encoder_ctl(e, OPUS_GET_LOOKAHEAD(&v));
	if (err != OPUS_OK) {
		return err;
	}
	return v;
}

static int mc_opus_encode(OpusEncoder *e, const short *pcm, int frame_size, unsigned char *out, int cap) {
	return opus_encode(e, pcm, frame_size, out, cap);
}

static int mc_opus_decoder_new(int rate, int channels, OpusDecoder **out) {
	int err = OPUS_OK;
	OpusDecoder *d = opus_decoder_create(rate, channels, &err);
	if (err != OPUS_OK) {
		*out = NULL;
		return err;
	}
	*out = d;
	return OPUS_OK;
}

static int mc_opus_decode(OpusDecoder *d, const unsigned char *data, int len, short *pcm, int frame_size, int fec) {
	return opus_decode(d, data, len, pcm, frame_size, fec);
}

static const char *mc_opus_strerror(int err) {
	return opus_strerror(err);
}
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"
)

// Application selects the libopus tuning mode.
type Application int

const (
	// ApplicationVoIP favours intelligibility and enables LBRR redundancy.
	ApplicationVoIP Application = C.OPUS_APPLICATION_VOIP
	// ApplicationAudio favours faithfulness.
	ApplicationAudio Application = C.OPUS_APPLICATION_AUDIO
)

// Sample rates libopus accepts.
const (
	MinSampleRate = 8000
	MaxSampleRate = 48000
)

// MaxFrameSamples is the longest frame libopus encodes or decodes, 120 ms at
// 48 kHz.
const MaxFrameSamples = 5760

// ErrClosed is returned after Close.
var ErrClosed = errors.New("opus: closed")

// Encoder wraps an OpusEncoder.
type Encoder struct {
	enc      *C.OpusEncoder
	channels int
	rate     int
	closed   bool
}

// NewEncoder creates an encoder for rate and channels.  bitrate is in bits
// per second; fec enables in-band redundancy and lossPerc primes it.
func NewEncoder(rate, channels, bitrate int, app Application, fec bool, lossPerc int) (*Encoder, error) {
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("opus: unsupported channel count %d", channels)
	}
	if rate < MinSampleRate || rate > MaxSampleRate {
		return nil, fmt.Errorf("opus: unsupported sample rate %d", rate)
	}
	var enc *C.OpusEncoder
	if err := C.mc_opus_encoder_new(C.int(rate), C.int(channels), C.int(app), &enc); err != 0 {
		return nil, fmt.Errorf("opus: creating encoder: %w", codeError(int(err)))
	}
	e := &Encoder{enc: enc, channels: channels, rate: rate}
	if err := e.ctl(ctlBitrate, bitrate); err != nil {
		e.Close()
		return nil, err
	}
	loss := 0
	if fec {
		loss = lossPerc
		if loss <= 0 {
			loss = 20
		}
		if err := e.ctl(ctlInbandFEC, 1); err != nil {
			e.Close()
			return nil, err
		}
	}
	if err := e.ctl(ctlPacketLoss, loss); err != nil {
		e.Close()
		return nil, err
	}
	if err := e.ctl(ctlComplexity, 5); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

// Encoder control selectors, mirrored as plain C functions because the
// OPUS_SET_* macros cannot be referenced through cgo.
const (
	ctlBitrate = iota
	ctlInbandFEC
	ctlPacketLoss
	ctlComplexity
)

func (e *Encoder) ctl(which, value int) error {
	if e == nil || e.closed {
		return ErrClosed
	}
	if err := C.mc_opus_ctl(e.enc, C.int(which), C.int(value)); err != 0 {
		return fmt.Errorf("opus: encoder control %d: %w", which, codeError(int(err)))
	}
	return nil
}

// Lookahead returns the encoder delay in samples at the stream rate.
func (e *Encoder) Lookahead() int {
	if e == nil || e.closed {
		return 0
	}
	n := C.mc_opus_lookahead(e.enc)
	if n < 0 {
		return 0
	}
	return int(n)
}

// Encode compresses one interleaved int16 frame.  frameSize is samples per
// channel and must be a valid Opus frame size (2.5 to 120 ms).
func (e *Encoder) Encode(pcm []int16, frameSize int) ([]byte, error) {
	if e == nil || e.closed {
		return nil, ErrClosed
	}
	if frameSize <= 0 || len(pcm) < frameSize*e.channels {
		return nil, fmt.Errorf("opus: encode needs %d samples per channel, got %d values", frameSize, len(pcm))
	}
	out := make([]byte, MaxFrameSamples*e.channels*2+1024)
	n := C.mc_opus_encode(e.enc, (*C.short)(unsafe.Pointer(&pcm[0])), C.int(frameSize),
		(*C.uchar)(unsafe.Pointer(&out[0])), C.int(len(out)))
	if n < 0 {
		return nil, fmt.Errorf("opus: encoding frame: %w", codeError(int(n)))
	}
	return out[:n], nil
}

// Close frees the encoder; it is safe to call twice.
func (e *Encoder) Close() {
	if e == nil || e.closed {
		return
	}
	C.opus_encoder_destroy(e.enc)
	e.enc = nil
	e.closed = true
}

// Decoder wraps an OpusDecoder.
type Decoder struct {
	dec      *C.OpusDecoder
	channels int
	rate     int
	closed   bool
}

// NewDecoder creates a decoder for rate and channels.
func NewDecoder(rate, channels int) (*Decoder, error) {
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("opus: unsupported channel count %d", channels)
	}
	if rate < MinSampleRate || rate > MaxSampleRate {
		return nil, fmt.Errorf("opus: unsupported sample rate %d", rate)
	}
	var dec *C.OpusDecoder
	if err := C.mc_opus_decoder_new(C.int(rate), C.int(channels), &dec); err != 0 {
		return nil, fmt.Errorf("opus: creating decoder: %w", codeError(int(err)))
	}
	return &Decoder{dec: dec, channels: channels, rate: rate}, nil
}

// Decode decodes data into interleaved int16 PCM.  A nil or empty data slice
// asks libopus for packet loss concealment; fec asks it to use the in-band
// redundancy carried by the following packet.  The returned slice holds
// n*channels values.
func (d *Decoder) Decode(data []byte, frameSize int, fec bool) ([]int16, error) {
	if d == nil || d.closed {
		return nil, ErrClosed
	}
	if frameSize <= 0 || frameSize > MaxFrameSamples {
		return nil, fmt.Errorf("opus: frame size %d outside 1..%d", frameSize, MaxFrameSamples)
	}
	pcm := make([]int16, frameSize*d.channels)
	var ptr *C.uchar
	var length C.int
	if len(data) > 0 {
		ptr = (*C.uchar)(unsafe.Pointer(&data[0]))
		length = C.int(len(data))
	}
	fecFlag := C.int(0)
	if fec {
		fecFlag = 1
	}
	n := C.mc_opus_decode(d.dec, ptr, length, (*C.short)(unsafe.Pointer(&pcm[0])), C.int(frameSize), fecFlag)
	if n < 0 {
		return nil, fmt.Errorf("opus: decoding frame: %w", codeError(int(n)))
	}
	return pcm[:int(n)*d.channels], nil
}

// Close frees the decoder; it is safe to call twice.
func (d *Decoder) Close() {
	if d == nil || d.closed {
		return
	}
	C.opus_decoder_destroy(d.dec)
	d.dec = nil
	d.closed = true
}

// OpusHead builds the 19-byte identification header Matroska expects in the
// codec private data of an Opus track.
func OpusHead(channels, preSkip, inputRate int) []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1              // version
	head[9] = byte(channels) // channel count
	binary.LittleEndian.PutUint16(head[10:12], uint16(preSkip))
	binary.LittleEndian.PutUint32(head[12:16], uint32(inputRate))
	// Output gain and channel mapping family stay zero for mono/stereo.
	return head
}

type opusError int

func (e opusError) Error() string {
	return C.GoString(C.mc_opus_strerror(C.int(e)))
}

func codeError(code int) error { return opusError(code) }
