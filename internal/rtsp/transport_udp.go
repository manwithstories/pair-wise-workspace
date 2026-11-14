package rtsp

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// udpQueueDepth bounds the in-process datagram backlog. Combined with the
// kernel receive buffer it keeps bursty loopback streams lossless while the
// main goroutine parses and persists frames.
const udpQueueDepth = 4096

// UDPTransport receives RTP packets on a dedicated loopback UDP port. A
// second socket is opened for the RTCP port pair required by SETUP, although
// this capture pipeline only reassembles the RTP channel. A background reader
// drains the socket continuously so parsing/framing cannot cause drops.
type UDPTransport struct {
	rtpConn   *net.UDPConn
	rtcpConn  *net.UDPConn
	timeout   time.Duration
	queue     chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// NewUDPTransport binds two adjacent loopback UDP ports and returns the
// transport plus the RTP/RTCP client port numbers for the SETUP header. A few
// retries handle the rare case where the adjacent RTCP port is already taken.
func NewUDPTransport(requestedRTPPort int, timeout time.Duration) (*UDPTransport, [2]int, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		rtpConn, err := net.ListenUDP("udp",
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: requestedRTPPort})
		if err != nil {
			lastErr = err
			continue
		}
		_ = rtpConn.SetReadBuffer(8 * 1024 * 1024)
		rtpPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp",
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpPort + 1})
		if err != nil {
			_ = rtpConn.Close()
			lastErr = err
			continue
		}
		t := &UDPTransport{
			rtpConn:  rtpConn,
			rtcpConn: rtcpConn,
			timeout:  timeout,
			queue:    make(chan []byte, udpQueueDepth),
			done:     make(chan struct{}),
		}
		go t.readLoop()
		return t, [2]int{rtpPort, rtpPort + 1}, nil
	}
	return nil, [2]int{}, fmt.Errorf("bind udp port pair after retries: %w", lastErr)
}

func (t *UDPTransport) readLoop() {
	defer t.shutdown()
	for {
		buf := make([]byte, 65535)
		n, _, err := t.rtpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-t.done:
			default:
				t.queue <- nil // signals a permanent socket error to readers
			}
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		select {
		case t.queue <- pkt:
		case <-t.done:
			return
		}
	}
}

// ReadPacket returns the next queued RTP datagram, honoring the read deadline
// via a timer rather than a blocking socket call.
func (t *UDPTransport) ReadPacket() ([]byte, error) {
	if t.timeout > 0 {
		select {
		case pkt := <-t.queue:
			if pkt == nil {
				return nil, &TransportError{Detail: "udp socket closed"}
			}
			return pkt, nil
		case <-time.After(t.timeout):
			return nil, ReadTimeout{}
		}
	}
	pkt := <-t.queue
	if pkt == nil {
		return nil, &TransportError{Detail: "udp socket closed"}
	}
	return pkt, nil
}

// ClientPorts reports the bound RTP/RTCP port pair.
func (t *UDPTransport) ClientPorts() [2]int {
	rtpPort := t.rtpConn.LocalAddr().(*net.UDPAddr).Port
	return [2]int{rtpPort, rtpPort + 1}
}

// shutdown closes the done channel exactly once, safe to call from both the
// reader goroutine (on socket error) and an explicit Close.
func (t *UDPTransport) shutdown() {
	t.closeOnce.Do(func() { close(t.done) })
}

// Close stops the background reader and releases both sockets.
func (t *UDPTransport) Close() error {
	t.shutdown()
	err1 := t.rtpConn.Close()
	if t.rtcpConn != nil {
		if err2 := t.rtcpConn.Close(); err2 != nil && err1 == nil {
			return err2
		}
	}
	return err1
}
