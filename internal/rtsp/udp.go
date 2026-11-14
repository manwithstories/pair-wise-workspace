package rtsp

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/pion/rtcp"

	"rtspreplay/internal/sdp"
)

const dialTimeout = 5 * time.Second

// udpTrack holds the sockets of one negotiated track.
type udpTrack struct {
	idx        int
	payloadTyp uint8
	rtpConn    *net.UDPConn
	rtcpConn   *net.UDPConn
	serverRTP  *net.UDPAddr
	serverRTCP *net.UDPAddr
	rtcpSent   int64
	rtcpRecv   int64
}

// UDP is the RTP-over-UDP unicast transport. RTP and RTCP use an even/odd
// port pair per track; the control conversation stays on TCP.
type UDP struct {
	addr   string
	cc     *controlConn
	tracks []*udpTrack
	closed atomic.Bool
	wg     sync.WaitGroup
}

// NewUDP returns a UDP transport for the given host:port RTSP address.
func NewUDP(addr string) *UDP { return &UDP{addr: addr} }

// Connect dials the control connection.
func (t *UDP) Connect() error {
	cc, err := dialControl(t.addr)
	if err != nil {
		return err
	}
	t.cc = cc
	return nil
}

// Do sends one request on the shared control connection.
func (t *UDP) Do(req *base.Request) (*base.Response, error) { return t.cc.do(req) }

// SetupTrack allocates an even/odd port pair and negotiates the track.
func (t *UDP) SetupTrack(track *sdp.Track, sessionID string) (string, error) {
	rtpConn, rtcpConn, err := allocPortPair()
	if err != nil {
		return "", err
	}
	ut := &udpTrack{
		idx:        track.Index,
		payloadTyp: track.PayloadType,
		rtpConn:    rtpConn,
		rtcpConn:   rtcpConn,
	}
	rport := rtpConn.LocalAddr().(*net.UDPAddr).Port
	cport := rtcpConn.LocalAddr().(*net.UDPAddr).Port

	req := &base.Request{
		Method: base.Setup,
		URL:    mustParseURL(track.URL),
		Header: base.Header{
			"Transport": base.HeaderValue{fmt.Sprintf(
				"RTP/AVP/UDP;unicast;client_port=%d-%d", rport, cport)},
		},
	}
	if sessionID != "" {
		req.Header["Session"] = base.HeaderValue{sessionID}
	}
	res, err := t.Do(req)
	if err != nil {
		rtpConn.Close()
		rtcpConn.Close()
		return "", err
	}
	if res.StatusCode != base.StatusOK {
		rtpConn.Close()
		rtcpConn.Close()
		return "", fmt.Errorf("rtsp: SETUP %s: status %d", track.URL, res.StatusCode)
	}
	host, _, err := net.SplitHostPort(t.cc.nc.RemoteAddr().String())
	if err != nil {
		host = "127.0.0.1"
	}
	sr, sc := parseServerPorts(res.Header["Transport"])
	ut.serverRTP = &net.UDPAddr{IP: net.ParseIP(host), Port: sr}
	ut.serverRTCP = &net.UDPAddr{IP: net.ParseIP(host), Port: sc}
	t.tracks = append(t.tracks, ut)
	return parseSessionID(res), nil
}

// StartRTP spawns the RTP/RTCP reader goroutines and sends an initial RTCP
// Receiver Report per track.
func (t *UDP) StartRTP(onPkt PacketHandler, onErr ErrorHandler) error {
	for _, ut := range t.tracks {
		t.wg.Add(2)
		go t.rtpLoop(ut, onPkt, onErr)
		go t.rtcpLoop(ut)
		t.sendReceiverReport(ut)
	}
	return nil
}

// Teardown sends a best-effort TEARDOWN with a short deadline.
func (t *UDP) Teardown(sessionID string) {
	if t.cc == nil || t.cc.nc == nil {
		return
	}
	_ = t.cc.nc.SetDeadline(time.Now().Add(2 * time.Second))
	req := &base.Request{Method: base.Teardown, URL: mustParseURL(t.baseURL()),
		Header: base.Header{"Session": base.HeaderValue{sessionID}}}
	_, _ = t.cc.do(req)
}

// Close releases every socket and waits for the reader goroutines.
func (t *UDP) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	for _, ut := range t.tracks {
		ut.rtpConn.Close()
		ut.rtcpConn.Close()
	}
	if t.cc != nil {
		t.cc.close()
	}
	t.wg.Wait()
	return nil
}

func (t *UDP) baseURL() string { return "rtsp://" + t.addr + "/" }

func (t *UDP) rtpLoop(ut *udpTrack, onPkt PacketHandler, onErr ErrorHandler) {
	defer t.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, _, err := ut.rtpConn.ReadFromUDP(buf)
		if err != nil {
			if !t.closed.Load() && onErr != nil {
				onErr(err)
			}
			return
		}
		pkt := parseRTPPacket(buf[:n])
		if pkt == nil || pkt.PayloadType != ut.payloadTyp {
			continue
		}
		onPkt(ut.idx, pkt)
	}
}

func (t *UDP) rtcpLoop(ut *udpTrack) {
	defer t.wg.Done()
	buf := make([]byte, 2048)
	for {
		_, _, err := ut.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		atomic.AddInt64(&ut.rtcpRecv, 1)
	}
}

func (t *UDP) sendReceiverReport(ut *udpTrack) {
	rr := &rtcp.ReceiverReport{SSRC: uint32(0x1000 + ut.idx)}
	raw, err := rr.Marshal()
	if err != nil {
		return
	}
	if _, err := ut.rtcpConn.WriteToUDP(raw, ut.serverRTCP); err == nil {
		atomic.AddInt64(&ut.rtcpSent, 1)
	}
}

// allocPortPair reserves an even RTP port and the odd RTCP port above it.
func allocPortPair() (rtpConn, rtcpConn *net.UDPConn, err error) {
	loopback := net.ParseIP("127.0.0.1")
	for i := 0; i < 64; i++ {
		rtpConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			return nil, nil, fmt.Errorf("rtsp: allocate RTP port: %w", err)
		}
		port := rtpConn.LocalAddr().(*net.UDPAddr).Port
		if port%2 != 0 {
			rtpConn.Close()
			continue
		}
		rtcpConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: port + 1})
		if err != nil {
			rtpConn.Close()
			continue
		}
		return rtpConn, rtcpConn, nil
	}
	return nil, nil, fmt.Errorf("rtsp: no even/odd port pair available")
}

// parseServerPorts extracts server_port=RTP-RTCP from a Transport response
// header, defaulting to 5004-5005 when absent.
func parseServerPorts(v base.HeaderValue) (rtpPort, rtcpPort int) {
	rtpPort, rtcpPort = 5004, 5005
	for _, h := range v {
		for _, part := range strings.Split(h, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "server_port=") {
				fmt.Sscanf(strings.TrimPrefix(part, "server_port="), "%d-%d", &rtpPort, &rtcpPort)
			}
		}
	}
	return rtpPort, rtcpPort
}
