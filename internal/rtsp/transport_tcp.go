package rtsp

import (
	"net"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/conn"
)

// TCPTransport reads RTP packets from RTSP interleaved frames on channel 0.
type TCPTransport struct {
	nc      net.Conn
	rc      *conn.Conn
	timeout time.Duration
}

// NewTCPTransport wraps an established RTSP control connection with a fresh
// buffered reader. Prefer NewTCPTransportFromConn when the same connection was
// already used for the RTSP handshake.
func NewTCPTransport(nc net.Conn, timeout time.Duration) *TCPTransport {
	return &TCPTransport{nc: nc, rc: conn.NewConn(nc), timeout: timeout}
}

// NewTCPTransportFromConn reuses an existing conn.Conn (and its buffered
// reader) so interleaved frames are parsed from the exact same byte stream as
// the preceding OPTIONS/SETUP messages.
func NewTCPTransportFromConn(nc net.Conn, rc *conn.Conn, timeout time.Duration) *TCPTransport {
	return &TCPTransport{nc: nc, rc: rc, timeout: timeout}
}

// ReadPacket returns the payload of the next interleaved frame on the RTP
// channel. RTCP frames (odd channel ids) are skipped.
func (t *TCPTransport) ReadPacket() ([]byte, error) {
	for {
		if t.timeout > 0 {
			_ = t.nc.SetReadDeadline(time.Now().Add(t.timeout))
		}
		what, err := t.rc.Read()
		if err != nil {
			if isTimeoutErr(err) {
				return nil, ReadTimeout{}
			}
			return nil, err
		}
		frame, ok := what.(*base.InterleavedFrame)
		if !ok {
			// A stray RTSP message on a data connection ends the stream.
			return nil, &StreamClosed{Reason: "non-interleaved message on data connection"}
		}
		if frame.Channel%2 == 1 {
			continue // RTCP channel: ignored
		}
		payload := make([]byte, len(frame.Payload))
		copy(payload, frame.Payload)
		return payload, nil
	}
}

// Close tears down the control connection.
func (t *TCPTransport) Close() error { return t.nc.Close() }

func isTimeoutErr(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// StreamClosed signals that the server ended the data stream.
type StreamClosed struct{ Reason string }

func (e *StreamClosed) Error() string { return "stream closed: " + e.Reason }
