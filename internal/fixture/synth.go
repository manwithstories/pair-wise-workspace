package fixture

import (
	"encoding/binary"
	"fmt"
	"image"
	"math"

	"github.com/asticode/go-astiav"
)

// applyColor tags the video encoder with a colour description so the colour
// conflict fault can be observed through real codec metadata.
func applyColor(cc *astiav.CodecContext, tag ColorTag) {
	switch tag {
	case ColorBT2020:
		cc.SetColorPrimaries(astiav.ColorPrimariesBt2020)
		cc.SetColorTransferCharacteristic(astiav.ColorTransferCharacteristicSmpte2084)
		cc.SetColorSpace(astiav.ColorSpaceBt2020Ncl)
		cc.SetColorRange(astiav.ColorRangeMpeg)
	default:
		cc.SetColorPrimaries(astiav.ColorPrimariesBt709)
		cc.SetColorTransferCharacteristic(astiav.ColorTransferCharacteristicBt709)
		cc.SetColorSpace(astiav.ColorSpaceBt709)
		cc.SetColorRange(astiav.ColorRangeMpeg)
	}
}

// writeVideo synthesises a deterministic moving-bar luma pattern for the whole
// segment and encodes it into the muxer.
func writeVideo(fc *astiav.FormatContext, cc *astiav.CodecContext, st *astiav.Stream, spec SegmentSpec) error {
	nFrames := int(math.Round(spec.DurationSec * float64(spec.FrameRate)))
	frame := astiav.AllocFrame()
	defer frame.Free()
	frame.SetWidth(spec.Width)
	frame.SetHeight(spec.Height)
	frame.SetPixelFormat(astiav.PixelFormatYuv420P)
	

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	for i := 0; i < nFrames; i++ {
		// Unref resets all frame fields, so reconfigure every iteration.
		frame.SetWidth(spec.Width)
		frame.SetHeight(spec.Height)
		frame.SetPixelFormat(astiav.PixelFormatYuv420P)
		frame.SetPts(int64(i))
		if i == 0 {
			frame.SetFlags(astiav.NewFrameFlags(astiav.FrameFlagKey))
			frame.SetPictureType(astiav.PictureTypeI)
		}
		if err := frame.AllocBuffer(0); err != nil {
			return fmt.Errorf("frame %d alloc: %w", i, err)
		}
		if err := frame.ImageFillBlack(); err != nil {
			return fmt.Errorf("frame %d fill: %w", i, err)
		}
		if err := paintLuma(frame, spec, i); err != nil {
			return fmt.Errorf("frame %d paint: %w", i, err)
		}
		if err := drainVideoEncode(fc, cc, st, frame, pkt, false); err != nil {
			return fmt.Errorf("frame %d encode: %w", i, err)
		}
		frame.Unref()
	}
	return drainVideoEncode(fc, cc, st, nil, pkt, true)
}

// paintLuma builds a YCbCr image with a deterministic moving-bar luma pattern
// and neutral chroma, then writes it back into the frame via FromImage. Using
// image.YCbCr lets go-astiav handle the (possibly aligned) plane strides.
func paintLuma(frame *astiav.Frame, spec SegmentSpec, i int) error {
	w, h := spec.Width, spec.Height
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	barWidth := w / 8
	if barWidth < 1 {
		barWidth = 1
	}
	shift := (spec.SegmentIndex*7 + i) % w
	// Base brightness alternates strongly with the segment index so distinct
	// segments always encode to distinct bytes (fingerprint discrimination).
	base := byte(35 + (spec.SegmentIndex%4)*45)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			phase := (x + shift) / barWidth
			img.Y[y*img.YStride+x] = byte((int(base) + phase*18) % 256)
		}
	}
	// Neutral chroma (the value FFmpeg uses for black in MPEG range).
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	return frame.Data().FromImage(img)
}

func drainVideoEncode(fc *astiav.FormatContext, cc *astiav.CodecContext, st *astiav.Stream, frame *astiav.Frame, pkt *astiav.Packet, flush bool) error {
	if !flush {
		if err := cc.SendFrame(frame); err != nil {
			return err
		}
	} else {
		if err := cc.SendFrame(nil); err != nil {
			return err
		}
	}
	for {
		err := cc.ReceivePacket(pkt)
		if err != nil {
			if err == astiav.ErrEof || err == astiav.ErrEagain {
				return nil
			}
			return err
		}
		pkt.SetStreamIndex(st.Index())
		pkt.RescaleTs(cc.TimeBase(), st.TimeBase())
		if err := fc.WriteInterleavedFrame(pkt); err != nil {
			return err
		}
		pkt.Unref()
	}
}

// writeAudio synthesises AAC-frame-aligned planar float samples. AAC uses
// 1024 samples per frame; tone frequency and amplitude are deterministic.
func writeAudio(fc *astiav.FormatContext, cc *astiav.CodecContext, st *astiav.Stream, spec SegmentSpec) error {
	const frameLen = 1024
	total := int(math.Round(spec.DurationSec * float64(spec.SampleRate)))
	frame := astiav.AllocFrame()
	defer frame.Free()
	frame.SetChannelLayout(audioLayout(spec.Channels))
	frame.SetSampleRate(spec.SampleRate)
	frame.SetSampleFormat(astiav.SampleFormatFltp)
	
	frame.SetNbSamples(frameLen)

	pkt := astiav.AllocPacket()
	defer pkt.Free()

	pts := 0
	produced := 0
	for produced < total {
		n := frameLen
		if produced+n > total {
			n = total - produced
		}
		// Unref resets frame fields; reconfigure before each AllocBuffer.
		frame.SetChannelLayout(audioLayout(spec.Channels))
		frame.SetSampleRate(spec.SampleRate)
		frame.SetSampleFormat(astiav.SampleFormatFltp)
		frame.SetNbSamples(n)
		frame.SetPts(int64(pts))
		if err := frame.AllocBuffer(0); err != nil {
			return fmt.Errorf("audio frame alloc: %w", err)
		}
		if spec.SilentAudio {
			if err := frame.SamplesFillSilence(); err != nil {
				return fmt.Errorf("audio fill silence: %w", err)
			}
		} else {
			if err := paintTone(frame, spec, pts, n); err != nil {
				return fmt.Errorf("audio paint: %w", err)
			}
		}
		if err := drainAudioEncode(fc, cc, st, frame, pkt, false); err != nil {
			return fmt.Errorf("audio encode: %w", err)
		}
		pts += n
		produced += n
		frame.Unref()
	}
	return drainAudioEncode(fc, cc, st, nil, pkt, true)
}

func paintTone(frame *astiav.Frame, spec SegmentSpec, startSample, n int) error {
	// Read the current (silence-filled) buffer to learn the exact plane layout,
	// mutate it, then write it back with SetBytes since Bytes() returns a copy.
	buf, err := frame.Data().Bytes(0)
	if err != nil {
		return err
	}
	// For planar audio every plane uses the same stride; go-astiav reports it
	// only in linesize[0] (the remaining entries are zero). Using linesize[ch]
	// would collapse all planes onto plane 0 and produce NaN for >2 channels.
	plane := frame.Linesize()[0]
	if plane <= 0 {
		plane = n * 4
	}
	freq := 220.0 + float64(spec.SegmentIndex%5)*110.0
	for ch := 0; ch < spec.Channels; ch++ {
		base := ch * plane
		for i := 0; i < n; i++ {
			idx := base + i*4
			if idx+4 > len(buf) {
				break
			}
			// Slight per-channel phase offset keeps channels distinguishable.
			t := float64(startSample+i) / float64(spec.SampleRate)
			v := 0.35 * math.Sin(2*math.Pi*freq*t+float64(ch)*0.4)
			binary.LittleEndian.PutUint32(buf[idx:idx+4], math.Float32bits(float32(v)))
		}
	}
	return frame.Data().SetBytes(buf, 0)
}

func drainAudioEncode(fc *astiav.FormatContext, cc *astiav.CodecContext, st *astiav.Stream, frame *astiav.Frame, pkt *astiav.Packet, flush bool) error {
	if !flush {
		if err := cc.SendFrame(frame); err != nil {
			return err
		}
	} else {
		if err := cc.SendFrame(nil); err != nil {
			return err
		}
	}
	for {
		err := cc.ReceivePacket(pkt)
		if err != nil {
			if err == astiav.ErrEof || err == astiav.ErrEagain {
				return nil
			}
			return err
		}
		pkt.SetStreamIndex(st.Index())
		pkt.RescaleTs(cc.TimeBase(), st.TimeBase())
		if err := fc.WriteInterleavedFrame(pkt); err != nil {
			return err
		}
		pkt.Unref()
	}
}
