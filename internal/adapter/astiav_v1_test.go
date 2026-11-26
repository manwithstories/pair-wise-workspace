package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asticode/go-astiav"

	"archivemedia/internal/model"
)

// makeMKV 用 go-astiav 合成一个小型 FFV1+PCM 的 matroska 文件。
func makeMKV(t *testing.T, path string, videoFrames int) {
	t.Helper()
	fc, err := astiav.AllocOutputFormatContext(nil, "matroska", path)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Free()
	vc := astiav.FindEncoder(astiav.CodecIDFfv1)
	ac := astiav.FindEncoder(astiav.CodecIDPcmS16Le)
	vcc := astiav.AllocCodecContext(vc)
	defer vcc.Free()
	vcc.SetWidth(32)
	vcc.SetHeight(24)
	vcc.SetPixelFormat(astiav.PixelFormatYuv420P)
	vcc.SetTimeBase(astiav.NewRational(1, 25))
	if err := vcc.Open(vc, nil); err != nil {
		t.Fatal(err)
	}
	acc := astiav.AllocCodecContext(ac)
	defer acc.Free()
	acc.SetSampleFormat(astiav.SampleFormatS16)
	acc.SetSampleRate(8000)
	acc.SetChannelLayout(astiav.ChannelLayoutMono)
	acc.SetTimeBase(astiav.NewRational(1, 8000))
	if err := acc.Open(ac, nil); err != nil {
		t.Fatal(err)
	}
	vs, as := fc.NewStream(vc), fc.NewStream(ac)
	if err := vcc.ToCodecParameters(vs.CodecParameters()); err != nil {
		t.Fatal(err)
	}
	if err := acc.ToCodecParameters(as.CodecParameters()); err != nil {
		t.Fatal(err)
	}
	vs.SetTimeBase(vcc.TimeBase())
	as.SetTimeBase(acc.TimeBase())
	io, err := astiav.OpenIOContext(path, astiav.NewIOContextFlags(astiav.IOContextFlagWrite), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc.SetPb(io)
	if err := fc.WriteHeader(nil); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < videoFrames; i++ {
		f := astiav.AllocFrame()
		f.SetWidth(32)
		f.SetHeight(24)
		f.SetPixelFormat(astiav.PixelFormatYuv420P)
		if err := f.AllocBuffer(1); err != nil {
			t.Fatal(err)
		}
		f.SetPts(int64(i))
		f.ImageFillBlack()
		if err := vcc.SendFrame(f); err != nil {
			t.Fatal(err)
		}
		drainEnc(t, vcc, fc, vs)
		f.Free()
	}
	for total := 0; total < 4000; {
		n := 1000
		if total+n > 4000 {
			n = 4000 - total
		}
		f := astiav.AllocFrame()
		f.SetNbSamples(n)
		f.SetSampleFormat(astiav.SampleFormatS16)
		f.SetChannelLayout(astiav.ChannelLayoutMono)
		f.SetSampleRate(8000)
		if err := f.AllocBuffer(0); err != nil {
			t.Fatal(err)
		}
		f.SetPts(int64(total))
		f.SamplesFillSilence()
		if err := acc.SendFrame(f); err != nil {
			t.Fatal(err)
		}
		drainEnc(t, acc, fc, as)
		f.Free()
		total += n
	}
	vcc.SendFrame(nil)
	drainEnc(t, vcc, fc, vs)
	acc.SendFrame(nil)
	drainEnc(t, acc, fc, as)
	if err := fc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
}

func drainEnc(t *testing.T, cc *astiav.CodecContext, fc *astiav.FormatContext, st *astiav.Stream) {
	t.Helper()
	for {
		p := astiav.AllocPacket()
		if err := cc.ReceivePacket(p); err != nil {
			p.Free()
			return
		}
		p.RescaleTs(cc.TimeBase(), st.TimeBase())
		p.SetStreamIndex(st.Index())
		if err := fc.WriteInterleavedFrame(p); err != nil {
			t.Fatal(err)
		}
		p.Free()
	}
}

func TestAstiavParseRealContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.mkv")
	makeMKV(t, path, 20)
	parser := &AstiavParser{}
	ok, err := parser.Probe(path)
	if err != nil || !ok {
		t.Fatalf("probe ok=%v err=%v", ok, err)
	}
	// arc1 探针不得把 mkv 认成 arc1
	if ok2, _ := (&Arc1Parser{}).Probe(path); ok2 {
		t.Fatal("arc1 探针误命中 mkv")
	}
	pm, err := parser.Parse(path, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pm.AdapterName != "astiav" || len(pm.Tracks) != 2 {
		t.Fatalf("tracks=%d adapter=%s", len(pm.Tracks), pm.AdapterName)
	}
	var vPackets, aPackets int
	for _, pkt := range pm.Packets {
		if pm.Tracks[pkt.TrackIndex].Type == MediaVideo {
			vPackets++
		} else {
			aPackets++
		}
		if pkt.ContentHash == 0 {
			t.Fatal("解码帧必须带内容指纹")
		}
	}
	if vPackets != 20 || aPackets == 0 {
		t.Fatalf("video packets=%d audio packets=%d", vPackets, aPackets)
	}
}

func TestAstiavRejectsMissingFile(t *testing.T) {
	_, err := (&AstiavParser{}).Parse(filepath.Join(t.TempDir(), "nope.mkv"), DefaultLimits())
	if model.KindOf(err) != model.KindInvalidArgument {
		t.Fatalf("缺失文件: %v", err)
	}
}

func TestNegotiatePicksCorrectAdapter(t *testing.T) {
	dir := t.TempDir()
	arcPath := filepath.Join(dir, "x.arc")
	mkvPath := filepath.Join(dir, "x.mkv")
	if err := os.WriteFile(arcPath, append([]byte(ARCMagic), 1, 0, 0, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	makeMKV(t, mkvPath, 4)
	p1, err := Negotiate(arcPath, []Capability{CapDemux})
	if err != nil || p1.Name() != ARCAdapterName {
		t.Fatalf("arc 协商=%v err=%v", p1, err)
	}
	p2, err := Negotiate(mkvPath, []Capability{CapDemux, CapDecode})
	if err != nil || p2.Name() != astiavName {
		t.Fatalf("mkv 协商=%v err=%v", p2, err)
	}
}
