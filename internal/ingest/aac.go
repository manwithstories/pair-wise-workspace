package ingest

import (
	"fmt"
	"math"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/model"
)

// aacEncoder wraps the native AAC encoder for fixture generation.  Its
// output keeps the ADTS headers the encoder writes, because the inspect stage
// parses exactly what a supplier would deliver; remux strips them when it
// writes Matroska.
type aacEncoder struct {
	cc         *astiav.CodecContext
	frame      *astiav.Frame
	packet     *astiav.Packet
	asc        []byte
	frameSize  int
	sampleRate int
	channels   int
}

func newAACEncoder(sampleRate, channels, bitrate int) (*aacEncoder, error) {
	codec := astiav.FindEncoderByName("aac")
	if codec == nil {
		return nil, fmt.Errorf("ingest: AAC encoder unavailable: %w", model.ErrUnsupported)
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, fmt.Errorf("ingest: allocating AAC context")
	}
	cc.SetSampleRate(sampleRate)
	cc.SetChannelLayout(channelLayout(channels))
	cc.SetSampleFormat(astiav.SampleFormatFltp)
	cc.SetBitRate(int64(bitrate))
	cc.SetTimeBase(astiav.NewRational(1, sampleRate))
	cc.SetFlags(astiav.NewCodecContextFlags(astiav.CodecContextFlagBitexact))
	if err := cc.Open(codec, nil); err != nil {
		cc.Free()
		return nil, fmt.Errorf("ingest: opening AAC encoder: %w", err)
	}
	frame := astiav.AllocFrame()
	frame.SetSampleRate(sampleRate)
	frame.SetChannelLayout(channelLayout(channels))
	frame.SetSampleFormat(astiav.SampleFormatFltp)
	frame.SetNbSamples(cc.FrameSize())
	if err := frame.AllocBuffer(0); err != nil {
		frame.Free()
		cc.Free()
		return nil, fmt.Errorf("ingest: allocating AAC frame: %w", err)
	}
	return &aacEncoder{cc: cc, frame: frame, packet: astiav.AllocPacket(),
		asc: append([]byte(nil), cc.ExtraData()...), frameSize: cc.FrameSize(),
		sampleRate: sampleRate, channels: channels}, nil
}

// Extradata returns the AudioSpecificConfig Matroska stores.
func (a *aacEncoder) Extradata() []byte { return a.asc }

// Encode compresses one frame of planar float32 samples and returns every
// packet that became available.  The AAC encoder buffers internally, so the
// first calls return fewer packets than frames sent; Flush drains the tail.
func (a *aacEncoder) Encode(pcm [][]float32, samples int) ([][]byte, error) {
	flat := make([]float32, 0, samples*len(pcm))
	for _, plane := range pcm {
		flat = append(flat, plane...)
	}
	if err := a.frame.Data().SetBytes(f32Bytes(flat), 0); err != nil {
		return nil, fmt.Errorf("ingest: filling AAC frame: %w", err)
	}
	a.frame.SetPts(int64(0))
	if err := a.cc.SendFrame(a.frame); err != nil {
		return nil, fmt.Errorf("ingest: sending AAC frame: %w", err)
	}
	return a.drain()
}

// Flush signals end of input and returns the buffered packets.
func (a *aacEncoder) Flush() ([][]byte, error) {
	if err := a.cc.SendFrame(nil); err != nil {
		return nil, fmt.Errorf("ingest: flushing AAC encoder: %w", err)
	}
	return a.drain()
}

func (a *aacEncoder) drain() ([][]byte, error) {
	var out [][]byte
	for {
		if err := a.cc.ReceivePacket(a.packet); err != nil {
			// EOF and EAGAIN both mean no packet is available yet: a delayed
			// encoder needs more input before it emits anything.
			break
		}
		if len(a.packet.Data()) > 0 {
			// libavcodec's AAC encoder emits raw access units; suppliers
			// deliver ADTS-framed streams, so the fixture adds the header
			// the carriage layer would.
			out = append(out, addADTSHeader(a.packet.Data(), a.asc, a.sampleRate, a.channels))
		}
		a.packet.Unref()
	}
	return out, nil
}

// adtsRates maps an ADTS sampling frequency index to its rate.
var adtsRates = [13]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

// addADTSHeader wraps one raw AAC access unit in a 7 byte ADTS header.  The
// profile and channel configuration come from the AudioSpecificConfig the
// encoder produced, so the header and the Matroska codec private data agree.
func addADTSHeader(raw, asc []byte, sampleRate, channels int) []byte {
	profile := 1 // AAC-LC
	sfi := 3     // 48 kHz default
	if len(asc) >= 1 {
		profile = int(asc[0]>>6) & 0x03
		if p := int(asc[0]>>6) & 0x03; p > 0 {
			profile = p
		}
		if len(asc) >= 2 {
			sfi = int(asc[0]&0x07)<<1 | int(asc[1]>>7)
		}
	}
	for i, r := range adtsRates {
		if r == sampleRate {
			sfi = i
			break
		}
	}
	ch := channels
	if len(asc) >= 2 {
		if c := int(asc[1]>>3) & 0x0f; c > 0 {
			ch = c
		}
	}
	profileIndex := profile - 1 // ADTS stores profile minus one
	if profileIndex < 0 {
		profileIndex = 1
	}
	frameLen := len(raw) + 7
	out := make([]byte, 7, frameLen)
	out[0] = 0xFF
	out[1] = 0xF1 // MPEG-4, layer 0, no CRC
	out[2] = byte(profileIndex<<6) | byte(sfi<<2) | byte((ch>>2)&0x01)
	out[3] = byte((ch&0x03)<<6) | byte(frameLen>>11)
	out[4] = byte(frameLen >> 3)
	out[5] = byte((frameLen&0x07)<<5) | 0x1F
	out[6] = 0xFC
	return append(out, raw...)
}

// Close releases the encoder objects.
func (a *aacEncoder) Close() {
	if a == nil {
		return
	}
	if a.packet != nil {
		a.packet.Free()
		a.packet = nil
	}
	if a.frame != nil {
		a.frame.Free()
		a.frame = nil
	}
	if a.cc != nil {
		a.cc.Free()
		a.cc = nil
	}
}

func channelLayout(channels int) astiav.ChannelLayout {
	switch channels {
	case 1:
		return astiav.ChannelLayoutMono
	default:
		return astiav.ChannelLayoutStereo
	}
}

// f32Bytes reinterprets float32 samples as the little endian byte layout
// libavcodec expects for packed float formats.
func f32Bytes(pcm []float32) []byte {
	out := make([]byte, len(pcm)*4)
	for i, v := range pcm {
		bits := math.Float32bits(v)
		out[i*4] = byte(bits)
		out[i*4+1] = byte(bits >> 8)
		out[i*4+2] = byte(bits >> 16)
		out[i*4+3] = byte(bits >> 24)
	}
	return out
}
