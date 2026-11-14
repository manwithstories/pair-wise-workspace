package rtsp

import "time"

// Transport is the common surface of both RTP receive paths. Both the TCP
// interleaved and the dual-port UDP implementations emit identical raw RTP
// packets so the depacketizer and report stay transport-agnostic.
type Transport interface {
	// ReadPacket blocks until one RTP packet is available, the read deadline
	// fires (ErrReadTimeout) or the connection breaks.
	ReadPacket() ([]byte, error)
	// Close releases sockets.
	Close() error
}

// ReadTimeout is returned when no packet arrives within the configured window.
type ReadTimeout struct{}

func (ReadTimeout) Error() string { return "rtp read timeout" }

// TransportConfig carries the parameters shared by both transports.
type TransportConfig struct {
	Mode          string
	ReadTimeout   time.Duration
	ClientRTPPort int // 0 means pick an ephemeral UDP port
}
