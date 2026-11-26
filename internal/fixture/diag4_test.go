package fixture_test

import (
	"errors"
	"io"
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/ingest"
)

func TestDiagReadAll(t *testing.T) {
	frames := fixture.GenerateFrames(60, 41)
	audio := fixture.GenerateAudio(16, 41)
	canonical := fixture.BuildPackets(frames, audio)
	delivered, _ := fixture.InjectFaults(canonical, 60, 41)
	path := t.TempDir() + "/a.avp"
	hdr := ingest.Header{Frames: 60, FPS: 30, Width: fixture.Width, Height: fixture.Height, AudioRate: 48000, Packets: uint32(len(delivered))}
	ingest.WriteContainer(path, hdr, delivered)
	reader, _, _ := ingest.OpenPacketReader(path)
	defer reader.Close()
	n := 0
	for {
		_, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			t.Logf("clean EOF at n=%d", n)
			break
		}
		if err != nil {
			t.Logf("STOP at n=%d err=%v", n, err)
			break
		}
		n++
	}
}
