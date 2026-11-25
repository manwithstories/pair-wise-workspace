package fixture

import (
	"os"

	prtp "github.com/pion/rtp"

	"mediaaudit/internal/rtp"
)

// openRTP reads a generated RTP capture into an unwrapping Stream. It mirrors
// how the pipeline consumes the file and is shared by fixture tests.
func openRTP(path string) (*rtp.Stream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := rtp.NewStream("rtp", rtpClockRate)
	err = rtp.ReadPackets(f, func(p *prtp.Packet, wallNanos int64) error {
		s.Push(p.SequenceNumber, p.Timestamp, wallNanos)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}
