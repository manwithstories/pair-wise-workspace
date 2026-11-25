package fixture

import (
	"encoding/binary"
	"os"

	prtp "github.com/pion/rtp"
)

// RTP clock rate used for the proxy/commentary capture.
const rtpClockRate uint32 = 90000

// writeRTP emits the deterministic length-prefixed RTP capture consumed by
// internal/rtp. Timestamps start at WrapAt so the 32-bit counter wraps many
// times, while the device clock runs DriftPPM faster than wall time.
func writeRTP(path string, o Options) error {
	slope := 1 + o.DriftPPM/1e6
	// One packet per 10 ms wall clock; enough packets to cover the audio span.
	stepWallNs := int64(10_000_000)
	packets := int(o.Frames*1_000_000_000/o.SampleRate/stepWallNs) + 1

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var baseDev int64 = int64(o.WrapAt)
	for i := 0; i < packets; i++ {
		wall := int64(i) * stepWallNs
		devElapsed := slope*float64(wall)/1e9*float64(rtpClockRate) + 0.5
		ts := uint32(baseDev + int64(devElapsed))
		pkt := &prtp.Packet{
			Header: prtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: uint16(i % 65536),
				Timestamp:      ts,
				SSRC:           0x5151A,
				Marker:         i%25 == 0,
			},
			Payload: []byte{byte(i), byte(i >> 8), byte(i >> 16)},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return err
		}
		var wb [8]byte
		var lb [4]byte
		binary.LittleEndian.PutUint64(wb[:], uint64(wall))
		binary.LittleEndian.PutUint32(lb[:], uint32(len(raw)))
		if _, err := f.Write(wb[:]); err != nil {
			return err
		}
		if _, err := f.Write(lb[:]); err != nil {
			return err
		}
		if _, err := f.Write(raw); err != nil {
			return err
		}
	}
	return nil
}
