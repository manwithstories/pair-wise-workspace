package fixture_test

import (
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/ingest"
	"avpipe/internal/media"
	"avpipe/internal/transport"
)

func TestDiagFull(t *testing.T) {
	frames := fixture.GenerateFrames(60, 41)
	audio := fixture.GenerateAudio(16, 41)
	canonical := fixture.BuildPackets(frames, audio)
	delivered, _ := fixture.InjectFaults(canonical, 60, 41)
	path := t.TempDir() + "/a.avp"
	hdr := ingest.Header{Frames: 60, FPS: 30, Width: fixture.Width, Height: fixture.Height, AudioRate: 48000, Packets: uint32(len(delivered))}
	ingest.WriteContainer(path, hdr, delivered)
	reader, _, _ := ingest.OpenPacketReader(path)
	defer reader.Close()
	var pk []*media.Packet
	for {
		p, err := reader.ReadPacket()
		if err != nil {
			break
		}
		if me, ok := err.(*media.Error); ok && me.Class == media.ClassCorrupt {
			p.Corrupt = true
		}
		pk = append(pk, p)
	}
	t.Logf("read %d packets", len(pk))
	ordered, dedup, _ := transport.ReorderDedup(pk)
	t.Logf("dedup out=%d exact=%d conflict=%d", len(ordered), dedup.ExactDuplicates, dedup.ConflictingDuplicates)
	paritySeen := 0
	for _, p := range ordered {
		if p.Kind == media.KindParity {
			paritySeen++
			t.Logf("  parity after dedup: block=%d parityOf=%d", p.BlockID, p.ParityOf)
		}
	}
	t.Logf("parity count after dedup=%d", paritySeen)
}
