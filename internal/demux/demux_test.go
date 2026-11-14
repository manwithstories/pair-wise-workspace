package demux

import (
	"bytes"
	"io"
	"testing"
	"time"

	"tsprobe/internal/fixture"
	"tsprobe/internal/psi"
	"tsprobe/internal/tspacket"
)

func runStream(t *testing.T, data []byte) (*psi.Store, *Demuxer, tspacket.Stats, []Keyframe) {
	t.Helper()
	reader := tspacket.NewReader(bytes.NewReader(data), 8)
	store := psi.NewStore()
	dmx := New(store)
	var keyframes []Keyframe
	for {
		packet, err := reader.ReadPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		keyframe, err := dmx.Process(packet)
		if err != nil {
			t.Fatal(err)
		}
		if keyframe != nil {
			keyframes = append(keyframes, *keyframe)
		}
	}
	return store, dmx, reader.Stats(), keyframes
}

func TestFixtureDemuxCountsAndKeyframes(t *testing.T) {
	var buf bytes.Buffer
	_, err := fixture.Generate(&buf, fixture.Options{
		Programs: 2, Duration: 12 * time.Second, Loss: 7, CCJumps: 3, Versions: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, dmx, packetStats, keyframes := runStream(t, buf.Bytes())
	if packetStats.CCJumps != 3 || packetStats.LostPackets != 7 {
		t.Fatalf("packet stats=%+v", packetStats)
	}
	if stats := dmx.Stats(); stats.CRCFailures != 0 || stats.Keyframes != 3 {
		t.Fatalf("demux stats=%+v", stats)
	}
	if len(keyframes) != 3 || keyframes[1].PTS <= keyframes[0].PTS || keyframes[2].PTS <= keyframes[1].PTS {
		t.Fatalf("keyframes=%+v", keyframes)
	}
	counts := store.PacketCounts()
	if counts[0x100] != 1180 || counts[0x1000] != 24 {
		t.Fatalf("packet counts=%v", counts)
	}
	if store.PATVersions() != 4 || len(store.PMTVersions()) != 1 || store.PMTVersions()[0].Versions != 3 {
		t.Fatalf("versions pat=%d pmt=%+v", store.PATVersions(), store.PMTVersions())
	}
}

func buildPSIPacket(pid uint16, cc uint8, section []byte) []byte {
	packet := make([]byte, 188)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = 0x47
	packet[1] = byte(pid>>8) | 0x40
	packet[2] = byte(pid)
	packet[3] = 0x10 | cc&0x0f
	packet[4] = 0
	copy(packet[5:], section)
	return packet
}

func TestPESIDRWithoutRandomAccessFlag(t *testing.T) {
	pat, _ := psi.BuildPAT(1, 0, []psi.PATProgram{{Program: 1, PID: 0x1000}})
	pmt, _ := psi.BuildPMT(1, 0x100, 0, []psi.ESInfo{{StreamType: 0x1b, ElementaryPID: 0x100}}, nil)
	pts := int64(90000)
	ptsBytes := []byte{
		0x21 | byte((pts>>29)&0x0e), byte(pts >> 22),
		0x01 | byte((pts>>14)&0xfe), byte(pts >> 7), 0x01 | byte((pts<<1)&0xfe),
	}
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5}
	pes = append(pes, ptsBytes...)
	pes = append(pes, 0, 0, 1, 0x65, 1, 2, 3)
	video := make([]byte, 188)
	for i := range video {
		video[i] = 0xff
	}
	video[0] = 0x47
	video[1] = 0x41
	video[2] = 0x00
	video[3] = 0x10
	copy(video[4:], pes)
	data := bytes.Join([][]byte{buildPSIPacket(0, 0, pat), buildPSIPacket(0x1000, 0, pmt), video}, nil)
	_, _, _, keyframes := runStream(t, data)
	if len(keyframes) != 1 || !keyframes[0].HasPTS {
		t.Fatalf("IDR keyframe not detected: %+v", keyframes)
	}
}
