package source

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"

	astiav "github.com/asticode/go-astiav"

	"mediapipe/internal/media"
)

type SyntheticConfig struct {
	Path        string `json:"path"`
	Seconds     int    `json:"seconds"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Seed        int64  `json:"seed"`
}

func (c SyntheticConfig) Normalize() SyntheticConfig {
	if c.Seconds <= 0 {
		c.Seconds = 2
	}
	if c.Width <= 0 {
		c.Width = 320
	}
	if c.Height <= 0 {
		c.Height = 180
	}
	c.Width &^= 1
	c.Height &^= 1
	if c.FPS <= 0 {
		c.FPS = 15
	}
	if c.BitrateKbps <= 0 {
		c.BitrateKbps = 500
	}
	return c
}

// GenerateSyntheticMP4 creates a deterministic H.264 MP4 entirely through
// go-astiav. It is used because the task forbids external fixture files.
func GenerateSyntheticMP4(cfg SyntheticConfig) error {
	cfg = cfg.Normalize()
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return media.WrapError(media.CodeIO, "create fixture directory", err)
	}
	astiav.SetLogLevel(astiav.LogLevelError)
	oc, err := astiav.AllocOutputFormatContext(nil, "mp4", cfg.Path)
	if err != nil {
		return media.WrapError(media.CodeEncode, "alloc fixture muxer", err)
	}
	defer oc.Free()
	codec := astiav.FindEncoder(astiav.CodecIDH264)
	if codec == nil {
		return media.NewError(media.CodeEncode, "h264 encoder unavailable", nil)
	}
	cc := astiav.AllocCodecContext(codec)
	defer cc.Free()
	cc.SetWidth(cfg.Width)
	cc.SetHeight(cfg.Height)
	cc.SetPixelFormat(astiav.PixelFormatYuv420P)
	cc.SetTimeBase(astiav.NewRational(1, cfg.FPS))
	cc.SetBitRate(int64(cfg.BitrateKbps) * 1000)
	cc.SetGopSize(cfg.FPS)
	cc.SetMaxBFrames(0)
	if oc.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		cc.SetFlags(cc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}
	if err := cc.Open(codec, nil); err != nil {
		return media.WrapError(media.CodeEncode, "open h264 encoder", err)
	}
	st := oc.NewStream(nil)
	if st == nil {
		return media.NewError(media.CodeEncode, "create fixture stream", nil)
	}
	if err := st.CodecParameters().FromCodecContext(cc); err != nil {
		return media.WrapError(media.CodeEncode, "copy fixture codec parameters", err)
	}
	st.SetTimeBase(cc.TimeBase())
	ioCtx, err := astiav.OpenIOContext(cfg.Path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		return media.WrapError(media.CodeIO, "open fixture io", err)
	}
	defer ioCtx.Close()
	oc.SetPb(ioCtx)
	if err := oc.WriteHeader(nil); err != nil {
		return media.WrapError(media.CodeEncode, "write fixture header", err)
	}
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frames := cfg.Seconds * cfg.FPS
	for i := 0; i < frames; i++ {
		f := syntheticFrame(cfg, i)
		if err := encodeDrain(cc, oc, st, f, pkt); err != nil {
			f.Free()
			return err
		}
		f.Free()
	}
	if err := encodeDrain(cc, oc, st, nil, pkt); err != nil {
		return err
	}
	if err := oc.WriteTrailer(); err != nil {
		return media.WrapError(media.CodeEncode, "write fixture trailer", err)
	}
	if err := ioCtx.Close(); err != nil {
		return media.WrapError(media.CodeIO, "close fixture io", err)
	}
	return nil
}

func syntheticFrame(cfg SyntheticConfig, i int) *astiav.Frame {
	f := astiav.AllocFrame()
	f.SetWidth(cfg.Width)
	f.SetHeight(cfg.Height)
	f.SetPixelFormat(astiav.PixelFormatYuv420P)
	f.SetPts(int64(i))
	if err := f.AllocBuffer(0); err != nil {
		panic(fmt.Sprintf("alloc frame buffer: %v", err))
	}
	img := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	phase := int(cfg.Seed) + i*3
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			block := ((x/24 + y/24 + phase/9) % 2) * 180
			gradient := (x + y + phase*7) % 76
			img.SetRGBA(x, y, color.RGBA{R: uint8(block + gradient%60), G: uint8(80 + (x+phase)%120),
				B: uint8(120 + (y+phase)%100), A: 255})
		}
	}
	if err := f.Data().FromImage(img); err != nil {
		panic(fmt.Sprintf("fill frame: %v", err))
	}
	return f
}

func encodeDrain(cc *astiav.CodecContext, oc *astiav.FormatContext, st *astiav.Stream, f *astiav.Frame, pkt *astiav.Packet) error {
	if err := cc.SendFrame(f); err != nil {
		return media.WrapError(media.CodeEncode, "send frame", err)
	}
	for {
		err := cc.ReceivePacket(pkt)
		if err != nil {
			if errorsIsEAGAINorEOF(err) {
				return nil
			}
			return media.WrapError(media.CodeEncode, "receive encoded packet", err)
		}
		pkt.SetStreamIndex(st.Index())
		pkt.RescaleTs(cc.TimeBase(), st.TimeBase())
		if err := oc.WriteInterleavedFrame(pkt); err != nil {
			pkt.Unref()
			return media.WrapError(media.CodeEncode, "write encoded packet", err)
		}
		pkt.Unref()
	}
}

func errorsIsEAGAINorEOF(err error) bool {
	return errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof)
}
