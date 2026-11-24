package demux

import (
	"encoding/binary"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// MP4TrackInfo describes one track discovered in the init segment.
type MP4TrackInfo struct {
	SourceID  uint32
	Kind      media.Kind
	Timescale uint32
	Width     uint32
	Height    uint32
}

// rmtaPayload is the private "relay metadata, per fragment" box carried inside
// each traf. Standard players ignore unknown boxes; the relay reader uses it to
// recover capture anchors (clock ticks and reference nanoseconds).
type rmtaPayload struct {
	Sequence     uint64
	ClockTicks   uint64
	CaptureRefNS int64
	TimescaleHz  uint32
}

// rmtaBoxType is the private relay metadata box carried inside each traf.
func rmtaBoxType() mp4.BoxType { return mp4.StrToBoxType("rmta") }

func encodeRMTA(p rmtaPayload) []byte {
	b := make([]byte, 32)
	bin := binary.BigEndian
	bin.PutUint64(b[0:8], p.Sequence)
	bin.PutUint64(b[8:16], p.ClockTicks)
	bin.PutUint64(b[16:24], uint64(p.CaptureRefNS))
	bin.PutUint32(b[24:28], p.TimescaleHz)
	return b
}

func decodeRMTA(b []byte) (rmtaPayload, error) {
	if len(b) < 28 {
		return rmtaPayload{}, media.NewError(media.ErrMetadata, 0, "short rmta box")
	}
	bin := binary.BigEndian
	return rmtaPayload{
		Sequence:     bin.Uint64(b[0:8]),
		ClockTicks:   bin.Uint64(b[8:16]),
		CaptureRefNS: int64(bin.Uint64(b[16:24])),
		TimescaleHz:  bin.Uint32(b[24:28]),
	}, nil
}
