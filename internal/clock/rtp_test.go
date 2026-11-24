package clock

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func TestRTPExtractorVideo90kPassthrough(t *testing.T) {
	x, err := NewRTPExtractor(90_000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.ToTick90k(123_456)
	if err != nil || got != 123_456 {
		t.Fatalf("90k video must pass through, got %d err %v", got, err)
	}
}

func TestRTPExtractorAudio48kScaling(t *testing.T) {
	// 48kHz 音频：48000 个时钟单位 = 1 秒 = 90000 Tick。
	x, err := NewRTPExtractor(48_000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.ToTick90k(48_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 90_000 {
		t.Fatalf("audio scaling = %d, want 90000", got)
	}
}

func TestRTPExtractorRejectsZeroClock(t *testing.T) {
	if _, err := NewRTPExtractor(0); !errors.Is(err, ErrRTPTimestamp) {
		t.Fatalf("want ErrRTPTimestamp, got %v", err)
	}
	x := &RTPExtractor{}
	if _, err := x.ToTick90k(1); !errors.Is(err, ErrRTPTimestamp) {
		t.Fatalf("want ErrRTPTimestamp on zero clock, got %v", err)
	}
}

func TestRTPExtractorFromPacket(t *testing.T) {
	x, _ := NewRTPExtractor(90_000)
	p := &rtp.Packet{
		Header:  rtp.Header{Timestamp: 777, SSRC: 1, Version: 2},
		Payload: []byte{1, 2, 3},
	}
	got, err := x.PacketTimestamp(p)
	if err != nil || got != 777 {
		t.Fatalf("packet timestamp = %d err %v", got, err)
	}
	if _, err := x.PacketTimestamp(nil); !errors.Is(err, ErrRTPTimestamp) {
		t.Fatal("nil packet must error")
	}
}

func TestUnwrapRTPAcross32BitBoundary(t *testing.T) {
	x, _ := NewRTPExtractor(90_000)
	// 32 位 RTP 时间戳在 2^32 附近回绕（33 位空间可完整表示）。
	near := uint32(0xFFFF_FFFF - 45_000 + 1)
	ticks, wraps, err := x.UnwrapRTP([]uint32{near, near + 45_000, 45_000})
	if err != nil {
		t.Fatal(err)
	}
	if wraps != 1 {
		t.Fatalf("wraps = %d, want 1", wraps)
	}
	for i := 1; i < len(ticks); i++ {
		if ticks[i] <= ticks[i-1] {
			t.Fatalf("not monotone: %v", ticks)
		}
	}
}
