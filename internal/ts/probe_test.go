package ts

import (
	"bytes"
	"testing"
	"time"
)

func TestProbePCRAndIDRWithoutRandomAccessFlag(t *testing.T) {
	frameTicks := DurationToTicks(time.Second / 30)
	var stream bytes.Buffer
	stream.Write(packetWithPCR(0, false, true))
	stream.Write(packetWithPCR(frameTicks, false, false))
	probe, err := ProbeReader(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Packets != 2 || !probe.HasPCR || len(probe.Keyframes) != 1 {
		t.Fatalf("unexpected probe: %+v", probe)
	}
	if diff := probe.Duration() - 2*time.Duration(frameTicks)*time.Second/27_000_000; diff < -time.Millisecond || diff > time.Millisecond {
		t.Fatalf("duration = %s", probe.Duration())
	}
}

func packetWithPCR(ticks int64, randomAccess, idr bool) []byte {
	packet := make([]byte, PacketSize)
	packet[0] = 0x47
	packet[1] = 0x01
	packet[3] = 0x30
	packet[4] = 7
	packet[5] = 0x10
	if randomAccess {
		packet[5] |= 0x40
	}
	base := uint64(ticks / 300)
	ext := uint16(ticks % 300)
	packet[6] = byte(base >> 25)
	packet[7] = byte(base >> 17)
	packet[8] = byte(base >> 9)
	packet[9] = byte(base >> 1)
	packet[10] = byte(base&1)<<7 | byte(ext>>8)
	packet[11] = byte(ext)
	if idr {
		copy(packet[13:], []byte{0x00, 0x00, 0x00, 0x01, 0x65})
	}
	return packet
}
