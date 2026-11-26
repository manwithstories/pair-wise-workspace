// mkv.go 用 go-astiav 生成一个真实 matroska 样例（FFV1 + PCM），
// 验证 astiav 适配器端到端参与归档流水线。
package fixture

import (
	"os"
	"path/filepath"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/model"
)

// GenerateMKV 在 dir/sources 下生成 real.mkv 并返回对应 Source。
func GenerateMKV(dir string) (Source, error) {
	srcDir := filepath.Join(dir, "sources")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return Source{}, model.Wrap(model.KindPathViolation, err, "fixture: mkdir %s", srcDir)
	}
	path := filepath.Join(srcDir, "real.mkv")
	if err := muxMKV(path, 25); err != nil {
		return Source{}, err
	}
	return Source{
		Kind: KindRealMKV, Path: path,
		Plan: CutPlan{PlanRanges: []PlanRange{
			{0, frameUs(0), frameUs(8), model.RoundNearest, false},
		}},
	}, nil
}

func muxMKV(path string, frames int) error {
	fc, err := astiav.AllocOutputFormatContext(nil, "matroska", path)
	if err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "fixture: alloc output")
	}
	defer fc.Free()
	vc, ac := astiav.FindEncoder(astiav.CodecIDFfv1), astiav.FindEncoder(astiav.CodecIDPcmS16Le)
	vcc := astiav.AllocCodecContext(vc)
	defer vcc.Free()
	vcc.SetWidth(64)
	vcc.SetHeight(48)
	vcc.SetPixelFormat(astiav.PixelFormatYuv420P)
	vcc.SetTimeBase(astiav.NewRational(1, videoTB))
	if err := vcc.Open(vc, nil); err != nil {
		return model.Wrap(model.KindUnsupportedFormat, err, "fixture: open video enc")
	}
	acc := astiav.AllocCodecContext(ac)
	defer acc.Free()
	acc.SetSampleFormat(astiav.SampleFormatS16)
	acc.SetSampleRate(audioSR)
	acc.SetChannelLayout(astiav.ChannelLayoutMono)
	acc.SetTimeBase(astiav.NewRational(1, audioSR))
	if err := acc.Open(ac, nil); err != nil {
		return model.Wrap(model.KindUnsupportedFormat, err, "fixture: open audio enc")
	}
	vs, as := fc.NewStream(vc), fc.NewStream(ac)
	if err := vcc.ToCodecParameters(vs.CodecParameters()); err != nil {
		return err
	}
	if err := acc.ToCodecParameters(as.CodecParameters()); err != nil {
		return err
	}
	vs.SetTimeBase(vcc.TimeBase())
	as.SetTimeBase(acc.TimeBase())
	io, err := astiav.OpenIOContext(path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "fixture: open io")
	}
	fc.SetPb(io)
	if err := fc.WriteHeader(nil); err != nil {
		return model.Wrap(model.KindCorruptMedia, err, "fixture: header")
	}
	for i := 0; i < frames; i++ {
		f := astiav.AllocFrame()
		f.SetWidth(64)
		f.SetHeight(48)
		f.SetPixelFormat(astiav.PixelFormatYuv420P)
		if err := f.AllocBuffer(1); err != nil {
			return err
		}
		f.SetPts(int64(i))
		f.ImageFillBlack()
		if err := vcc.SendFrame(f); err != nil {
			return err
		}
		drain(vcc, fc, vs)
		f.Free()
	}
	for total := 0; total < audioSR/2; {
		n := 1000
		if total+n > audioSR/2 {
			n = audioSR/2 - total
		}
		f := astiav.AllocFrame()
		f.SetNbSamples(n)
		f.SetSampleFormat(astiav.SampleFormatS16)
		f.SetChannelLayout(astiav.ChannelLayoutMono)
		f.SetSampleRate(audioSR)
		if err := f.AllocBuffer(0); err != nil {
			return err
		}
		f.SetPts(int64(total))
		f.SamplesFillSilence()
		if err := acc.SendFrame(f); err != nil {
			return err
		}
		drain(acc, fc, as)
		f.Free()
		total += n
	}
	vcc.SendFrame(nil)
	drain(vcc, fc, vs)
	acc.SendFrame(nil)
	drain(acc, fc, as)
	return model.Wrap(model.KindCorruptMedia, fc.WriteTrailer(), "fixture: trailer")
}

func drain(cc *astiav.CodecContext, fc *astiav.FormatContext, st *astiav.Stream) {
	for {
		p := astiav.AllocPacket()
		if err := cc.ReceivePacket(p); err != nil {
			p.Free()
			return
		}
		p.RescaleTs(cc.TimeBase(), st.TimeBase())
		p.SetStreamIndex(st.Index())
		if err := fc.WriteInterleavedFrame(p); err != nil {
			p.Free()
			return
		}
		p.Free()
	}
}
