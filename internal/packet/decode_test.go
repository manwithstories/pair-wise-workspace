package packet

import (
	"math"
	"testing"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/media"
)

// quietFFmpeg 抑制 astiav 在测试中向 stderr 打印 C 层日志。
func quietFFmpeg() {
	astiav.SetLogLevel(astiav.LogLevelQuiet)
}

// testEncoder 在测试内生成真实 Opus/AAC 裸包，不依赖外部文件。
type testEncoder struct {
	codec     Codec
	ctx       *astiav.CodecContext
	frame     *astiav.Frame
	pkt       *astiav.Packet
	rate      int
	extraData []byte
}

func newTestEncoder(t *testing.T, codec Codec, rate int) *testEncoder {
	t.Helper()
	quietFFmpeg()
	var id astiav.CodecID
	if codec == CodecOpus {
		id = astiav.CodecIDOpus
	} else {
		id = astiav.CodecIDAac
	}
	c := astiav.FindEncoder(id)
	if c == nil {
		t.Skipf("编码器 %s 不可用，跳过", codec)
	}
	ctx := astiav.AllocCodecContext(c)
	if ctx == nil {
		t.Fatal("编码上下文分配失败")
	}
	ctx.SetSampleRate(rate)
	ctx.SetChannelLayout(astiav.ChannelLayoutStereo)
	ctx.SetBitRate(64000)
	// libopus 只接受交错 flt/s16；原生 AAC 接受平面 fltp。
	if codec == CodecOpus {
		ctx.SetSampleFormat(astiav.SampleFormatFlt)
	} else {
		ctx.SetSampleFormat(astiav.SampleFormatFltp)
	}
	if err := ctx.Open(c, nil); err != nil {
		ctx.Free()
		t.Fatalf("打开编码器失败: %v", err)
	}
	return &testEncoder{
		codec: codec, ctx: ctx,
		frame: astiav.AllocFrame(), pkt: astiav.AllocPacket(), rate: rate,
		extraData: append([]byte(nil), ctx.ExtraData()...),
	}
}

func (en *testEncoder) close() {
	en.frame.Free()
	en.pkt.Free()
	en.ctx.Free()
}

// encodeTone 编码一段正弦音，返回每个输出裸包的载荷。
func (en *testEncoder) encodeTone(t *testing.T, frames int, freq float64) [][]byte {
	t.Helper()
	frameSize := en.ctx.FrameSize()
	if frameSize <= 0 {
		frameSize = 960
	}
	var out [][]byte
	step := 2 * math.Pi * freq / float64(en.rate)
	interleaved := en.codec == CodecOpus
	sf := astiav.SampleFormatFltp
	if interleaved {
		sf = astiav.SampleFormatFlt
	}
	for f := 0; f < frames; f++ {
		en.frame.Unref()
		en.frame.SetNbSamples(frameSize)
		en.frame.SetSampleFormat(sf)
		en.frame.SetSampleRate(en.rate)
		en.frame.SetChannelLayout(astiav.ChannelLayoutStereo)
		if err := en.frame.AllocBuffer(0); err != nil {
			t.Fatal(err)
		}
		buf := make([]float32, frameSize*2)
		for i := 0; i < frameSize; i++ {
			v := float32(0.2 * math.Sin(step*float64(f*frameSize+i)))
			if interleaved {
				buf[i*2] = v
				buf[i*2+1] = v
			} else {
				buf[i] = v
				buf[frameSize+i] = v
			}
		}
		if err := en.frame.Data().SetBytes(float32ToBytes(buf), 0); err != nil {
			t.Fatal(err)
		}
		if err := en.ctx.SendFrame(en.frame); err != nil {
			t.Fatalf("送帧编码失败: %v", err)
		}
		for {
			en.pkt.Unref()
			err := en.ctx.ReceivePacket(en.pkt)
			if err == astiav.ErrEagain || err == astiav.ErrEof {
				break
			}
			if err != nil {
				t.Fatalf("取包失败: %v", err)
			}
			out = append(out, append([]byte(nil), en.pkt.Data()...))
		}
	}
	return out
}

func float32ToBytes(s []float32) []byte {
	b := make([]byte, len(s)*4)
	for i, v := range s {
		bits := math.Float32bits(v)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}

func decodeRoundTrip(t *testing.T, codec Codec, rate int) {
	t.Helper()
	enc := newTestEncoder(t, codec, rate)
	defer enc.close()
	payloads := enc.encodeTone(t, 6, 440)
	if len(payloads) == 0 {
		t.Fatal("编码器未产出任何包")
	}
	dec, err := NewDecoder(codec, enc.extraData)
	if err != nil {
		t.Fatalf("构造解码器失败: %v", err)
	}
	defer dec.Close()

	decoded := 0
	for i, p := range payloads {
		e := &Event{
			StreamID: 1, Epoch: 0, Seq: uint16(i), Codec: codec,
			ClockRate: uint32(rate), DurationTicks: uint32(rate / 50),
			ArrivalIndex: i, Payload: p,
		}
		frame, err := dec.Decode(e)
		if err != nil {
			t.Fatalf("第 %d 包解码失败: %v", i, err)
		}
		if frame != nil {
			if frame.Channels != 2 || frame.SampleRate != rate || frame.NbSamples <= 0 {
				t.Fatalf("帧元数据异常: %+v", frame)
			}
			if len(frame.Samples) != frame.NbSamples*frame.Channels {
				t.Fatalf("样本长度 = %d", len(frame.Samples))
			}
			decoded++
		}
	}
	if decoded == 0 {
		t.Fatal("未解码出任何有效帧")
	}
}

func TestDecodeOpusRoundTrip(t *testing.T) {
	decodeRoundTrip(t, CodecOpus, 48000)
}

func TestDecodeAACRoundTrip(t *testing.T) {
	decodeRoundTrip(t, CodecAAC, 48000)
}

func TestDecodeCorruptPayloadReportsLocation(t *testing.T) {
	quietFFmpeg()
	dec, err := NewDecoder(CodecOpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	// 构造明显非法的 Opus 载荷。
	e := &Event{
		StreamID: 9, Epoch: 3, Seq: 55, Codec: CodecOpus,
		ClockRate: 48000, DurationTicks: 960, ArrivalIndex: 77,
		Payload: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
	}
	_, err = dec.Decode(e)
	if !media.IsKind(err, media.KindDecode) {
		t.Fatalf("损坏包应报 KindDecode，得到 %v", err)
	}
	var me *media.MediaError
	if !asMedia(err, &me) || me.Loc.StreamID != 9 || me.Loc.Seq != 55 ||
		me.Loc.ArrivalIndex != 77 {
		t.Fatalf("损坏包位置丢失: %v", err)
	}
}

func TestDecodePlaceholderRejected(t *testing.T) {
	dec, err := NewDecoder(CodecOpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	ph := &Event{
		StreamID: 1, Seq: 1, Codec: CodecOpus, Flags: FlagMissing,
		ClockRate: 48000, DurationTicks: 960, ArrivalIndex: 0,
	}
	if _, err := dec.Decode(ph); !media.IsKind(err, media.KindDecode) {
		t.Fatalf("占位事件必须拒绝解码: %v", err)
	}
}

func asMedia(err error, target **media.MediaError) bool {
	for err != nil {
		if me, ok := err.(*media.MediaError); ok {
			*target = me
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
