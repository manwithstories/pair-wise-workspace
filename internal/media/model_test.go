package media

import (
	"errors"
	"testing"
)

func TestMediaErrorWrapping(t *testing.T) {
	err := NewError(ErrChecksumMismatch, 7, "frame 3 bad")
	me := AsMediaError(err)
	if me == nil || me.Kind != ErrChecksumMismatch || me.Source != 7 {
		t.Fatalf("structured error wrong: %+v", me)
	}
	wrapped := errors.Join(errors.New("outer"), err)
	if got := AsMediaError(wrapped); got == nil || got.Kind != ErrChecksumMismatch {
		t.Fatalf("errors.As did not find MediaError: %v", wrapped)
	}
	if AsMediaError(errors.New("plain")) != nil {
		t.Fatal("plain error must not map to MediaError")
	}
}

func TestKindString(t *testing.T) {
	if KindVideo.String() != "video" || KindAudio.String() != "audio" || KindUnknown.String() != "unknown" {
		t.Fatal("kind string mapping wrong")
	}
}

func TestLimitsDocumented(t *testing.T) {
	if MaxFrameBytes == 0 || MaxPacketBytes == 0 || MaxTotalSamples == 0 || MaxSources == 0 {
		t.Fatal("sample limits must be non-zero constants")
	}
	if MaxPacketBytes > 65535 {
		t.Fatal("RTP length prefix is u16; packet cap must fit")
	}
}
