package remux

import (
	"testing"
)

func TestVerifyOverRTPSmallFrame(t *testing.T) {
	res, err := VerifyOverRTP([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	if err != nil {
		t.Fatalf("rtp: %v", err)
	}
	if !res.RoundTripOK || res.Packets != 1 || res.MarkerPackets != 1 {
		t.Fatalf("single-frame rtp wrong: %+v", res)
	}
}

func TestVerifyOverRTPFragmentedFrame(t *testing.T) {
	frame := make([]byte, maxRTPPayload*2+37)
	for i := range frame {
		frame[i] = byte(i * 7)
	}
	res, err := VerifyOverRTP(frame)
	if err != nil {
		t.Fatalf("rtp fragmented: %v", err)
	}
	if !res.RoundTripOK || res.Packets != 3 || res.MarkerPackets != 1 {
		t.Fatalf("fragmentation accounting wrong: %+v", res)
	}
}

func TestVerifyOverRTPEmptyFrame(t *testing.T) {
	res, err := VerifyOverRTP(nil)
	if err != nil || !res.RoundTripOK || res.Packets != 1 {
		t.Fatalf("empty frame handling wrong: %+v err=%v", res, err)
	}
}
