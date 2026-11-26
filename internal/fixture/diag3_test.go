package fixture_test

import (
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/ingest"
	"avpipe/internal/media"
)

func TestDiagRead(t *testing.T) {
	frames := fixture.GenerateFrames(60, 41)
	audio := fixture.GenerateAudio(16, 41)
	canonical := fixture.BuildPackets(frames, audio)
	delivered, _ := fixture.InjectFaults(canonical, 60, 41)
	kinds := map[uint8]int{}
	for _, p := range delivered {
		kinds[uint8(p.Kind)]++
	}
	t.Logf("before write: video=%d audio=%d parity=%d", kinds[1], kinds[2], kinds[3])
	path := t.TempDir() + "/a.avp"
	hdr := ingest.Header{Frames: 60, FPS: 30, Width: fixture.Width, Height: fixture.Height, AudioRate: 48000, Packets: uint32(len(delivered))}
	if err := ingest.WriteContainer(path, hdr, delivered); err != nil {
		t.Fatal(err)
	}
	reader, manifest, err := ingest.OpenPacketReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("manifest packets=%d", manifest.Packets)
	defer reader.Close()
	rkinds := map[uint8]int{}
	n := 0
	for {
		p, rerr := reader.ReadPacket()
		if rerr != nil {
			if me, ok := rerr.(*media.Error); ok {
				t.Logf("read error class=%s msg=%s", me.Class, me.Message)
			}
			break
		}
		rkinds[uint8(p.Kind)]++
		n++
	}
	t.Logf("after read n=%d video=%d audio=%d parity=%d", n, rkinds[1], rkinds[2], rkinds[3])
}
