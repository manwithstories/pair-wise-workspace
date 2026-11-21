package pipeline

import (
	"archivecheck.local/archivecheck/internal/remux"
	"archivecheck.local/archivecheck/internal/rtpprobe"
)

// rtpParams gives the packetizer plausible framing parameters. The check is
// about packet structure, not stream conformance, so a single payload type is
// used for every track.
const (
	rtpPayloadType = 96
	rtpClockRate   = 90000
	// rtpChunk bounds the bytes read from the spool per validation step.
	rtpChunk = 1200
)

// validateSpooledRTP re-reads each spooled sample in bounded chunks and checks
// that it frames into structurally valid RTP packets whose payload survives the
// round trip. Payloads are read one sample at a time, so memory stays bounded
// by the largest sample regardless of archive length.
func validateSpooledRTP(spool interface {
	ReadAt(p []byte, off int64) (int, error)
}, perTrack map[uint64][]remux.Sample) (int, error) {
	v := rtpprobe.NewValidator(rtpPayloadType, rtpClockRate, 0)
	index := 0
	for _, list := range perTrack {
		for _, s := range list {
			size := s.Size
			if s.Data != nil {
				size = int64(len(s.Data))
			}
			if size <= 0 {
				continue
			}
			// Read in MTU-sized chunks so even a large sample never forces a
			// full-sample allocation.
			for off := int64(0); off < size; off += rtpChunk {
				n := int64(rtpChunk)
				if off+n > size {
					n = size - off
				}
				buf := make([]byte, n)
				if _, err := spool.ReadAt(buf, s.Offset+off); err != nil {
					return 0, err
				}
				chunkIndex := index
				if off > 0 {
					// Continuation chunks belong to the same logical sample;
					// feeding them individually still exercises packet
					// framing and payload integrity.
					chunkIndex = index
				}
				if err := v.AddSample(buf, chunkIndex); err != nil {
					return 0, err
				}
			}
			index++
		}
	}
	return v.Result().Packets, nil
}
