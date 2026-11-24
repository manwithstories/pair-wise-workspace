package pack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
)

func TestWriteLPCMParsesAsIPCM(t *testing.T) {
	pcm := make([]byte, 4800) // 1200 stereo 16-bit frames
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "audio.mp4")
	prod, err := WriteLPCM(path, pcm, Options{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if prod.Frames != 1200 || prod.PCMBytes != len(pcm) || len(prod.SHA256) != 64 {
		t.Fatalf("bad product %+v", prod)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	boxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(),
		mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeIpcm(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 {
		t.Fatalf("ipcm boxes=%d", len(boxes))
	}
	ase, ok := boxes[0].Payload.(*mp4.AudioSampleEntry)
	if !ok {
		t.Fatalf("payload %T is not AudioSampleEntry", boxes[0].Payload)
	}
	if ase.ChannelCount != 2 || ase.SampleSize != 16 || ase.GetSampleRateInt() != 48000 {
		t.Fatalf("bad audio entry: ch=%d size=%d rate=%d", ase.ChannelCount, ase.SampleSize, ase.GetSampleRateInt())
	}

	pcmBoxes, err := mp4.ExtractBoxWithPayload(f, nil, mp4.BoxPath{
		mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(),
		mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeIpcm(), mp4.BoxTypePcmC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pcmBoxes) != 1 {
		t.Fatalf("pcmC boxes=%d", len(pcmBoxes))
	}
	cfg, ok := pcmBoxes[0].Payload.(*mp4.PcmC)
	if !ok || cfg.FormatFlags != 1 || cfg.PCMSampleSize != 16 {
		t.Fatalf("bad pcmC payload=%#v ok=%v", pcmBoxes[0].Payload, ok)
	}
}

func TestWriteLPCMDeterministicHash(t *testing.T) {
	pcm := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	a, err := WriteLPCM(filepath.Join(t.TempDir(), "a.mp4"), pcm, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := WriteLPCM(filepath.Join(t.TempDir(), "b.mp4"), pcm, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("hashes differ: %s != %s", a.SHA256, b.SHA256)
	}
}

func TestWriteLPCMRejectsMisalignedBytes(t *testing.T) {
	_, err := WriteLPCM(filepath.Join(t.TempDir(), "x.mp4"), []byte{1}, Options{Channels: 2})
	if err == nil {
		t.Fatal("misaligned PCM should fail")
	}
}
