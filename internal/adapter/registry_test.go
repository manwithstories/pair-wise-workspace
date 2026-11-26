package adapter_test

import (
	"errors"
	"testing"

	"tsproof/internal/adapter"
	"tsproof/internal/codec"
)

// stubAdapter 用于制造错误版本的适配器。
type stubAdapter struct {
	name    string
	major   uint16
	st      uint8
}

func (s stubAdapter) Name() string         { return s.name }
func (s stubAdapter) MajorVersion() uint16 { return s.major }
func (s stubAdapter) StreamType() uint8    { return s.st }
func (s stubAdapter) Decode(uint16, int, int64, []byte) (*adapter.Block, error) {
	return nil, nil
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := adapter.NewRegistry()
	r.MustRegister(codec.NewJPEG())
	r.MustRegister(codec.NewPCM())
	got, err := r.Lookup(0x80)
	if err != nil || got.Name() != "jpeg" {
		t.Fatalf("jpeg lookup: %v err=%v", got, err)
	}
	got, err = r.Lookup(0x81)
	if err != nil || got.Name() != "pcm" {
		t.Fatalf("pcm lookup: %v err=%v", got, err)
	}
	if sts := r.StreamTypes(); len(sts) != 2 || sts[0] != 0x80 || sts[1] != 0x81 {
		t.Fatalf("stream types not sorted: %v", sts)
	}
}

func TestUnknownStreamType(t *testing.T) {
	r := adapter.NewRegistry()
	if _, err := r.Lookup(0x77); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("want ErrUnknownAdapter, got %v", err)
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	r := adapter.NewRegistry()
	bad := stubAdapter{name: "future-codec", major: adapter.ContractMajor + 1, st: 0x90}
	if err := r.Register(bad); !errors.Is(err, adapter.ErrVersion) {
		t.Fatalf("want ErrVersion, got %v", err)
	}
	if _, err := r.Lookup(0x90); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatal("rejected adapter must not be registered")
	}
}

func TestDuplicateRegistrationRejected(t *testing.T) {
	r := adapter.NewRegistry()
	if err := r.Register(codec.NewJPEG()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(codec.NewJPEG()); err == nil {
		t.Fatal("duplicate stream_type must fail")
	}
}

func TestJPEGAdapterRoundTrip(t *testing.T) {
	a := codec.NewJPEG()
	// 最小合法 JPEG：SOI + 任意段 + EOI。
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0xFF, 0xD9}
	pes := codec.EncodeJPEGBlock(7, true, 160, 120, jpeg)
	blk, err := a.Decode(0x100, 0, 12345, pes)
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	if blk.Kind != adapter.KindImage || !blk.Key || blk.Width != 160 || blk.Height != 120 {
		t.Fatalf("jpeg block fields wrong: %+v", blk)
	}
	if blk.PTS27 != 12345 || blk.TrackID != 0x100 {
		t.Fatalf("jpeg block routing wrong: %+v", blk)
	}
}

func TestJPEGBadMagicRejected(t *testing.T) {
	a := codec.NewJPEG()
	pes := codec.EncodeJPEGBlock(1, true, 16, 16, []byte{0xFF, 0xD8, 0xFF, 0xD9})
	pes[0] ^= 0xFF
	if _, err := a.Decode(0x100, 0, 0, pes); !errors.Is(err, codec.ErrFrameHeader) {
		t.Fatalf("want ErrFrameHeader, got %v", err)
	}
}

func TestPCMAdapterRoundTrip(t *testing.T) {
	a := codec.NewPCM()
	samples := []int16{0, 100, -100, 32767, -32768}
	pes := codec.EncodePCMBlock(3, samples)
	blk, err := a.Decode(0x101, 0, 999, pes)
	if err != nil {
		t.Fatalf("pcm decode: %v", err)
	}
	if blk.Kind != adapter.KindAudio || blk.Samples != 5 || !blk.Key {
		t.Fatalf("pcm block fields wrong: %+v", blk)
	}
	if len(blk.Payload) != 10 {
		t.Fatalf("pcm payload bytes=%d", len(blk.Payload))
	}
}

func TestDimensionAndPayloadCaps(t *testing.T) {
	a := codec.NewJPEG()
	pes := codec.EncodeJPEGBlock(1, true, codec.MaxDimension+1, 16, []byte{0xFF, 0xD8, 0xFF, 0xD9})
	if _, err := a.Decode(0x100, 0, 0, pes); !errors.Is(err, codec.ErrFrameHeader) {
		t.Fatalf("oversized dimension must fail, got %v", err)
	}
	short := []byte{0x00, 0x01}
	if _, err := a.Decode(0x100, 0, 0, short); !errors.Is(err, codec.ErrFrameHeader) {
		t.Fatalf("short buffer must fail, got %v", err)
	}
}
