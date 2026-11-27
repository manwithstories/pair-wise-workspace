package ingest

import (
	"fmt"

	"github.com/asticode/go-astiav"

	"mediacheck/internal/model"
)

// h264Encoder is a thin, single-threaded wrapper around libx264 configured
// for byte-reproducible output: bitexact mode, a fixed GOP, no B frames, no
// lookahead and a single slice per frame.
type h264Encoder struct {
	cc        *astiav.CodecContext
	frame     *astiav.Frame
	packet    *astiav.Packet
	width     int
	height    int
	gop       int
	extradata []byte
}

func newH264Encoder(width, height, fpsNum, fpsDen, gop int) (*h264Encoder, error) {
	codec := astiav.FindEncoderByName("libx264")
	if codec == nil {
		return nil, fmt.Errorf("ingest: libx264 encoder unavailable: %w", model.ErrUnsupported)
	}
	if gop <= 0 {
		gop = 25
	}
	cc := astiav.AllocCodecContext(codec)
	if cc == nil {
		return nil, fmt.Errorf("ingest: allocating H.264 context")
	}
	cc.SetWidth(width)
	cc.SetHeight(height)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(astiav.NewRational(1, fpsNum))
	cc.SetFramerate(astiav.NewRational(fpsNum, fpsDen))
	cc.SetGopSize(gop)
	cc.SetMaxBFrames(0)
	cc.SetThreadCount(1)
	cc.SetFlags(astiav.NewCodecContextFlags(
		astiav.CodecContextFlagBitexact,
		astiav.CodecContextFlagGlobalHeader,
	))
	opts := astiav.NewDictionary()
	defer opts.Free()
	opts.Set("preset", "ultrafast", 0)
	opts.Set("tune", "zerolatency", 0)
	opts.Set("x264-params", fmt.Sprintf(
		"keyint=%d:min-keyint=%d:scenecut=0:threads=1:rc-lookahead=0:bframes=0:sliced-threads=0:annexb=1",
		gop, gop), 0)
	if err := cc.Open(codec, opts); err != nil {
		cc.Free()
		return nil, fmt.Errorf("ingest: opening libx264: %w", err)
	}
	frame := astiav.AllocFrame()
	frame.SetWidth(width)
	frame.SetHeight(height)
	frame.SetPixelFormat(astiav.PixelFormatYuv420P)
	if err := frame.AllocBuffer(0); err != nil {
		frame.Free()
		cc.Free()
		return nil, fmt.Errorf("ingest: allocating frame buffer: %w", err)
	}
	return &h264Encoder{
		cc:        cc,
		frame:     frame,
		packet:    astiav.AllocPacket(),
		width:     width,
		height:    height,
		gop:       gop,
		extradata: annexBToAVCConfig(cc.ExtraData()),
	}, nil
}

// annexBToAVCConfig turns libx264's Annex-B SPS/PPS pair into the avcC record
// Matroska requires.  A buffer that is already an avcC record is returned
// unchanged.
func annexBToAVCConfig(extra []byte) []byte {
	if len(extra) >= 7 && extra[0] == 1 && extra[1] != 0x67 && extra[1] != 0x42 {
		return append([]byte(nil), extra...)
	}
	var sps, pps []byte
	i := 0
	for {
		start, scLen := findStartCode(extra, i)
		if start < 0 {
			break
		}
		body := start + scLen
		next, _ := findStartCode(extra, body)
		if next < 0 {
			next = len(extra)
		}
		nal := extra[body:next]
		if len(nal) > 0 {
			switch nal[0] & 0x1f {
			case 7:
				sps = append([]byte(nil), nal...)
			case 8:
				pps = append([]byte(nil), nal...)
			}
		}
		i = next
	}
	if len(sps) < 4 || len(pps) == 0 {
		return append([]byte(nil), extra...)
	}
	out := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	out = append(out, sps...)
	out = append(out, 1, byte(len(pps)>>8), byte(len(pps)))
	out = append(out, pps...)
	return out
}

// Extradata returns the avcC record Matroska stores as codec private data.
func (h *h264Encoder) Extradata() []byte { return h.extradata }

// EncodeFrame renders frame index i as a moving luma ramp and returns the
// Annex-B access unit and whether it is a sync sample.
func (h *h264Encoder) EncodeFrame(i int, seed int64) ([]byte, bool, error) {
	img := h.frame.Data()
	ySize := h.width * h.height
	buf := make([]byte, ySize+ySize/2)
	shift := int(seed%64) + i*3
	for y := 0; y < h.height; y++ {
		row := buf[y*h.width : (y+1)*h.width]
		for x := range row {
			row[x] = byte((x + shift) & 0xff)
		}
	}
	for i := ySize; i < len(buf); i++ {
		buf[i] = 128
	}
	if err := img.SetBytes(buf, 0); err != nil {
		return nil, false, fmt.Errorf("ingest: filling frame: %w", err)
	}
	h.frame.SetPts(int64(i))
	if err := h.cc.SendFrame(h.frame); err != nil {
		return nil, false, fmt.Errorf("ingest: sending frame %d: %w", i, err)
	}
	var out []byte
	key := false
	for {
		err := h.cc.ReceivePacket(h.packet)
		if err != nil {
			break
		}
		out = append(out, h.packet.Data()...)
		if h.packet.Flags().Has(astiav.PacketFlagKey) {
			key = true
		}
		h.packet.Unref()
	}
	return out, key, nil
}

// Close releases every FFmpeg object owned by the encoder.
func (h *h264Encoder) Close() {
	if h == nil {
		return
	}
	if h.packet != nil {
		h.packet.Free()
		h.packet = nil
	}
	if h.frame != nil {
		h.frame.Free()
		h.frame = nil
	}
	if h.cc != nil {
		h.cc.Free()
		h.cc = nil
	}
}
