package rtsp

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// UDPTransport uses TCP for RTSP and a consecutive loopback UDP pair for media.
type UDPTransport struct {
	control   *TCPTransport
	rtpConn   *net.UDPConn
	rtcpConn  *net.UDPConn
	ports     [2]int
	silence   time.Duration
	media     chan []byte
	mediaDone chan struct{}
	done      chan struct{}
	doneOnce  sync.Once
	mediaOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	readErr   error
}

// DialUDP connects RTSP over loopback TCP and binds a local UDP RTP/RTCP pair.
func DialUDP(ctx context.Context, addr string, silence time.Duration) (*UDPTransport, error) {
	control, err := DialTCP(ctx, addr)
	if err != nil {
		return nil, err
	}
	rtpConn, rtcpConn, ports, err := bindUDPPair()
	if err != nil {
		control.Close()
		return nil, err
	}
	if silence <= 0 {
		silence = 2 * time.Second
	}
	u := &UDPTransport{
		control: control, rtpConn: rtpConn, rtcpConn: rtcpConn, ports: ports,
		silence: silence, media: make(chan []byte, 256),
		mediaDone: make(chan struct{}), done: make(chan struct{}),
	}
	go u.readRTP()
	go u.readRTCP()
	return u, nil
}

func bindUDPPair() (*net.UDPConn, *net.UDPConn, [2]int, error) {
	ip := net.IPv4(127, 0, 0, 1)
	var ports [2]int
	for attempt := 0; attempt < 100; attempt++ {
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err != nil {
			return nil, nil, ports, err
		}
		rtpPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: rtpPort + 1})
		if err == nil {
			ports = [2]int{rtpPort, rtpPort + 1}
			return rtpConn, rtcpConn, ports, nil
		}
		rtpConn.Close()
	}
	return nil, nil, ports, fmt.Errorf("could not bind consecutive UDP ports")
}

func (u *UDPTransport) readRTP() {
	buf := make([]byte, 65535)
	for {
		n, _, err := u.rtpConn.ReadFromUDP(buf)
		if err != nil {
			u.fail(err)
			return
		}
		data := append([]byte(nil), buf[:n]...)
		select {
		case u.media <- data:
		case <-u.done:
			return
		}
	}
}

func (u *UDPTransport) readRTCP() {
	buf := make([]byte, 65535)
	for {
		n, _, err := u.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			u.fail(err)
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			u.fail(fmt.Errorf("parse RTCP: %w", err))
			return
		}
		for _, packet := range packets {
			if _, ok := packet.(*rtcp.Goodbye); ok {
				u.mediaOnce.Do(func() { close(u.mediaDone) })
			}
		}
	}
}

// Ports returns the bound client RTP and RTCP ports.
func (u *UDPTransport) Ports() (int, int) { return u.ports[0], u.ports[1] }

// SilenceTimeout returns the configured silence threshold.
func (u *UDPTransport) SilenceTimeout() time.Duration { return u.silence }

// ApplyServerTransport validates the client pair echoed by the server.
func (u *UDPTransport) ApplyServerTransport(value string) error {
	params, err := ParseTransportParams(value)
	if err != nil {
		return err
	}
	if params.ClientPorts != u.ports {
		return fmt.Errorf("server echoed client_port %v, expected %v", params.ClientPorts, u.ports)
	}
	return nil
}

func (u *UDPTransport) SendControl(ctx context.Context, data []byte) error {
	return u.control.SendControl(ctx, data)
}

func (u *UDPTransport) ReceiveControl(ctx context.Context) ([]byte, error) {
	return u.control.ReceiveControl(ctx)
}

// ReceiveMedia waits for RTP, returning ErrUDPSilence on a quiet interval.
func (u *UDPTransport) ReceiveMedia(ctx context.Context) ([]byte, error) {
	select {
	case data := <-u.media:
		return data, nil
	default:
	}
	if u.isMediaDone() {
		return nil, io.EOF
	}
	timer := time.NewTimer(u.silence)
	defer timer.Stop()
	select {
	case data := <-u.media:
		return data, nil
	case <-u.mediaDone:
		select {
		case data := <-u.media:
			return data, nil
		default:
			return nil, io.EOF
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-u.control.done:
		return nil, u.control.terminalError()
	case <-u.done:
		return nil, u.terminalError()
	case <-timer.C:
		return nil, ErrUDPSilence
	}
}

func (u *UDPTransport) isMediaDone() bool {
	select {
	case <-u.mediaDone:
		return true
	default:
		return false
	}
}

func (u *UDPTransport) fail(err error) {
	u.mu.Lock()
	if u.readErr == nil {
		u.readErr = err
	}
	u.mu.Unlock()
	u.doneOnce.Do(func() { close(u.done) })
}

func (u *UDPTransport) terminalError() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.readErr != nil {
		return u.readErr
	}
	return io.EOF
}

// Close closes both UDP sockets and the RTSP control connection.
func (u *UDPTransport) Close() error {
	var err error
	u.closeOnce.Do(func() {
		u.doneOnce.Do(func() { close(u.done) })
		u.rtpConn.Close()
		u.rtcpConn.Close()
		err = u.control.Close()
	})
	return err
}
