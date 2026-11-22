package packager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

func TestPackageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := New(f, 48000, 2, 960, 960*2*2, 0)
	if err != nil {
		t.Fatal(err)
	}
	frame := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 960) // 960 帧 * 2ch * 2B
	for i := 0; i < 50; i++ {                                   // 50 帧 = 1 秒
		if err := p.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	res, err := p.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 48000 || res.Duration != clock.Second {
		t.Fatalf("res=%+v", res)
	}
	// 实际码率 = 总字节*8/1s
	wantKbps := float64(res.Bytes*8) / 1000
	if res.ActualKbps < wantKbps-0.01 || res.ActualKbps > wantKbps+0.01 {
		t.Fatalf("kbps=%f want~%f", res.ActualKbps, wantKbps)
	}
	// 用 go-mp4 读回 box 结构验证可解析
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var types []string
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		types = append(types, h.BoxInfo.Type.String())
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
			mp4.BoxTypeMinf(), mp4.BoxTypeDinf(), mp4.BoxTypeStbl():
			return h.Expand()
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, ty := range types {
		joined += ty + " "
	}
	for _, want := range []string{"ftyp", "mdat", "moov", "mvhd", "trak", "mdhd", "stsd", "stts", "stsz", "stco"} {
		if !bytes.Contains([]byte(joined), []byte(want)) {
			t.Fatalf("missing box %s in %s", want, joined)
		}
	}
}

func TestNonIntegralFrame(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "x.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, _ := New(f, 48000, 2, 960, 960*2*2, 0)
	err = p.WriteFrame([]byte{1, 2, 3})
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindPackage || fe.Segment != 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestDeterministicOutput(t *testing.T) {
	build := func() []byte {
		path := filepath.Join(t.TempDir(), "o.mp4")
		f, _ := os.Create(path)
		defer f.Close()
		p, _ := New(f, 48000, 2, 960, 960*2*2, 0)
		frame := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 960)
		for i := 0; i < 10; i++ {
			p.WriteFrame(frame)
		}
		if _, err := p.Close(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		return b
	}
	if !bytes.Equal(build(), build()) {
		t.Fatal("same input produced different MP4 bytes")
	}
}
