package demux

import (
	"errors"
	"testing"
	"time"

	"github.com/pairwise/mediapack/internal/timeline"
	"github.com/pairwise/mediapack/internal/transport"
)

func TestPESFragmentationAndPTS(t *testing.T) {
	media := BuildMediaFrame(42, true, make([]byte, 500)) // forces several TS packets
	pes := BuildPES(0xe0, 2*timeline.PCRHz+1800, media)

	d := NewDemuxer()
	if err := d.SetTrack(0x101, "h264-synthetic", 2); err != nil {
		t.Fatal(err)
	}

	var frame *Frame
	cc := uint8(0)
	first := true
	for start := 0; start < len(pes); start += 184 {
		end := min(start+184, len(pes))
		raw, err := transport.BuildPacket(transport.PacketOptions{
			PID: 0x101, PUSI: first, CC: cc, Payload: pes[start:end],
		})
		if err != nil {
			t.Fatal(err)
		}
		p, err := transport.ParsePacket(raw, int64(start))
		if err != nil {
			t.Fatal(err)
		}
		f, err := d.Push(p)
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			frame = f
		}
		first = false
		cc++
	}
	if frame == nil {
		t.Fatal("no frame assembled")
	}
	if !frame.KeyFrame || frame.Sequence != 42 || frame.PSIVersion != 2 {
		t.Fatalf("bad frame metadata: %+v", frame)
	}
	if frame.PTS != 2*time.Second+20*time.Millisecond {
		t.Fatalf("PTS=%s", frame.PTS)
	}
	if len(frame.Payload) != 500 {
		t.Fatalf("payload len=%d", len(frame.Payload))
	}
}

func TestUnsupportedCodecIsUnifiedError(t *testing.T) {
	d := NewDemuxer()
	err := d.SetTrack(0x102, "aac", 1)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v, want ErrUnsupported", err)
	}
	var te *timeline.Error
	if !errors.As(err, &te) || te.Kind != timeline.ErrUnsupportedCodec {
		t.Fatalf("error is not unified codec error: %v", err)
	}
}
