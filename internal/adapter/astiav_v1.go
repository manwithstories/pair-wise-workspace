// astiav_v1.go 是 go-astiav（FFmpeg n9.0 绑定）适配器，负责真实容器的
// 解封装与解码，产出与 arc1 同构的 ParsedMedia，使时间线及后续内核
// 无需感知 CGO 类型。
//
// 能力边界：
//   - 支持 FFmpeg 可探测的容器（mkv/mov/mp4/...）与编解码器；
//   - 时间戳回绕由容器/FFmpeg 在 packet 层处理，本适配器不声明 wrap_expand；
//   - 损坏尾包按 astiav 错误中断，保留此前已验证 packet 前缀；
//   - 内容指纹按解码帧/音频块字节做 fnv1a，保证同内容跨封装一致。
package adapter

import (
	"hash/fnv"
	"os"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/model"
)

const (
	astiavName    = "astiav"
	astiavVersion = "1.0.0"
)

// AstiavParser 是真实容器解析器。
type AstiavParser struct{}

var _ Parser = (*AstiavParser)(nil)

func init() { MustRegister(&AstiavParser{}) }

func (p *AstiavParser) Name() string    { return astiavName }
func (p *AstiavParser) Version() string { return astiavVersion }
func (p *AstiavParser) Capabilities() []Capability {
	return []Capability{CapDemux, CapDecode, CapTruncation}
}

// Probe 用 FFmpeg 的格式探针判断，arc1 魔数文件不归属本适配器。
func (p *AstiavParser) Probe(path string) (bool, error) {
	if ok, err := (&Arc1Parser{}).Probe(path); err != nil {
		return false, err
	} else if ok {
		return false, nil // arc1 交给专用适配器
	}
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return false, model.NewError(model.KindUnsupportedFormat, "astiav: alloc format context")
	}
	defer fc.Free()
	if err := fc.OpenInput(path, nil, nil); err != nil {
		return false, nil // 无法识别即不命中，交由协商报 unsupported
	}
	defer fc.CloseInput()
	return true, nil
}

// Parse 解封装并解码全部轨道（受 Limits 约束）。
func (p *AstiavParser) Parse(path string, opts ParseOptions) (*ParsedMedia, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, model.Wrap(model.KindInvalidArgument, err, "astiav: stat %s", path)
	}
	if opts.MaxFileBytes > 0 && info.Size() > opts.MaxFileBytes {
		return nil, model.NewError(model.KindQuotaExceeded,
			"astiav: file %d exceeds limit %d", info.Size(), opts.MaxFileBytes)
	}
	astiav.SetLogLevel(astiav.LogLevelError)
	fc := astiav.AllocFormatContext()
	if fc == nil {
		return nil, model.NewError(model.KindUnsupportedFormat, "astiav: alloc context")
	}
	defer fc.Free()
	if err := fc.OpenInput(path, nil, nil); err != nil {
		return nil, model.Wrap(model.KindUnsupportedFormat, err, "astiav: open %s", path)
	}
	defer fc.CloseInput()
	if err := fc.FindStreamInfo(nil); err != nil {
		return nil, model.Wrap(model.KindUnsupportedFormat, err, "astiav: stream info")
	}
	if opts.MaxTracks > 0 && fc.NbStreams() > opts.MaxTracks {
		return nil, model.NewError(model.KindQuotaExceeded,
			"astiav: %d streams exceed limit %d", fc.NbStreams(), opts.MaxTracks)
	}

	pm := &ParsedMedia{
		SourceID: path, AdapterName: astiavName, AdapterVersion: astiavVersion,
		Container: "ffmpeg", FileSize: info.Size(),
	}
	decoders, err := p.openDecoders(fc, pm)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, d := range decoders {
			d.Free()
		}
	}()

	var corrupt *model.MediaError
	for {
		pkt := astiav.AllocPacket()
		readErr := fc.ReadFrame(pkt)
		if readErr == astiav.ErrEof {
			pkt.Free()
			break
		}
		if readErr != nil {
			// 损坏：保留已验证前缀，记录首个坏时间。
			corrupt = model.Wrap(model.KindCorruptMedia, readErr, "astiav: read frame").(*model.MediaError)
			pkt.Free()
			break
		}
		si := pkt.StreamIndex()
		if si >= len(decoders) || decoders[si] == nil {
			pkt.Free()
			continue
		}
		// packet 级容器时间戳（pts/duration），frame 无 Duration 访问器。
		startTicks, pktDuration := pkt.Pts(), pkt.Duration()
		if err := p.decodePacket(decoders[si], pm, pkt, startTicks, pktDuration, opts); err != nil {
			corrupt, _ = err.(*model.MediaError)
			pkt.Free()
			break
		}
		pkt.Free()
	}
	if corrupt != nil {
		pm.Truncated = true
		if corrupt.FirstBadAt.IsZero() && len(pm.Packets) > 0 {
			last := pm.Packets[len(pm.Packets)-1]
			corrupt.FirstBadAt = model.TicksToTime(last.StartTicks, pm.Tracks[last.TrackIndex].TimeBase)
		}
		pm.FirstBadAt = corrupt.FirstBadAt
		return pm, corrupt
	}
	return pm, nil
}

// openDecoders 为每条流打开解码器并填充 Track 元数据。
func (p *AstiavParser) openDecoders(fc *astiav.FormatContext,
	pm *ParsedMedia) ([]*astiav.CodecContext, error) {
	decoders := make([]*astiav.CodecContext, fc.NbStreams())
	for _, st := range fc.Streams() {
		cp := st.CodecParameters()
		codec := astiav.FindDecoder(cp.CodecID())
		if codec == nil {
			return nil, model.NewError(model.KindUnsupportedFormat,
				"astiav: no decoder for codec id %d on stream %d", cp.CodecID(), st.Index())
		}
		cc := astiav.AllocCodecContext(codec)
		if err := cc.FromCodecParameters(cp); err != nil {
			cc.Free()
			return nil, model.Wrap(model.KindUnsupportedFormat, err, "astiav: codec params")
		}
		if err := cc.Open(codec, nil); err != nil {
			cc.Free()
			return nil, model.Wrap(model.KindUnsupportedFormat, err, "astiav: open decoder")
		}
		decoders[st.Index()] = cc
		tb := st.TimeBase()
		rtb, err := model.NewRational(int64(tb.Num()), int64(tb.Den()))
		if err != nil {
			cc.Free()
			return nil, model.Wrap(model.KindUnsupportedFormat, err, "astiav: timebase")
		}
		mt := MediaUnknown
		switch cp.MediaType() {
		case astiav.MediaTypeVideo:
			mt = MediaVideo
		case astiav.MediaTypeAudio:
			mt = MediaAudio
		}
		pm.Tracks = append(pm.Tracks, Track{
			ID: itoa(st.Index()), Index: st.Index(), Type: mt, TimeBase: rtb,
			Width: cp.Width(), Height: cp.Height(),
			SampleRate: cp.SampleRate(), Channels: cp.ChannelLayout().Channels(),
			Codec: codec.Name(),
		})
	}
	return decoders, nil
}

// decodePacket 送 packet 入解码器并把产出帧转成 FramePacket。
func (p *AstiavParser) decodePacket(cc *astiav.CodecContext, pm *ParsedMedia,
	pkt *astiav.Packet, startTicks, pktDuration int64, opts ParseOptions) error {
	si := pkt.StreamIndex()
	if err := cc.SendPacket(pkt); err != nil && err != astiav.ErrEagain {
		return model.Wrap(model.KindCorruptMedia, err, "astiav: send packet")
	}
	for {
		frame := astiav.AllocFrame()
		err := cc.ReceiveFrame(frame)
		if err == astiav.ErrEagain || err == astiav.ErrEof {
			frame.Free()
			return nil
		}
		if err != nil {
			frame.Free()
			return model.Wrap(model.KindCorruptMedia, err, "astiav: receive frame")
		}
		hash := frameContentHash(frame)
		pm.Packets = append(pm.Packets, FramePacket{
			TrackIndex:    si,
			StartTicks:    startTicks,
			DurationTicks: pktDuration,
			KeyFrame:      frame.PictureType() == astiav.PictureTypeI,
			ContentHash:   hash,
			PayloadSize:   frameDataSize(frame),
		})
		frame.Free()
		if opts.MaxDurationUs > 0 {
			last := pm.Packets[len(pm.Packets)-1]
			now := model.TicksToTime(last.StartTicks, pm.Tracks[si].TimeBase).Microseconds()
			if now > opts.MaxDurationUs {
				return model.NewError(model.KindQuotaExceeded,
					"astiav: duration %dus exceeds limit %d", now, opts.MaxDurationUs)
			}
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// frameContentHash 对解码帧可读字节做 fnv1a。
func frameContentHash(f *astiav.Frame) uint64 {
	h := fnv.New64a()
	if data, err := f.Data().Bytes(1); err == nil {
		h.Write(data)
	}
	return h.Sum64()
}

func frameDataSize(f *astiav.Frame) int {
	if data, err := f.Data().Bytes(1); err == nil {
		return len(data)
	}
	return 0
}
