package mockserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pion/rtcp"

	auditrtsp "rtspaudit/internal/rtsp"
)

func (s *session) handleSetup(req request) bool {
	if s.transportReady || (s.server.cfg.Policy == "strict" && !s.seenDESCRIBE) {
		s.writeResponse(req.cseq, "455 Method Not Valid in This State", nil, nil)
		return false
	}
	params, err := audittransport(req.headers["transport"])
	if err != nil {
		s.writeResponse(req.cseq, "461 Unsupported Transport", nil, nil)
		return false
	}
	headers := s.sessionHeaders()
	if strings.Contains(strings.ToUpper(params.Protocol), "TCP") {
		s.tcpMode = true
		headers["Transport"] = fmt.Sprintf(
			"RTP/AVP/TCP;unicast;interleaved=0-1;ssrc=%08x", ssrc)
	} else {
		if params.ClientPorts[0] == 0 {
			s.writeResponse(req.cseq, "461 Unsupported Transport", nil, nil)
			return false
		}
		rtpConn, rtcpConn, serverPorts, err := bindServerUDPPair()
		if err != nil {
			s.writeResponse(req.cseq, "500 Internal Server Error", nil, nil)
			return false
		}
		s.rtpConn, s.rtcpConn = rtpConn, rtcpConn
		remote, ok := s.conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			s.writeResponse(req.cseq, "500 Internal Server Error", nil, nil)
			return false
		}
		s.clientRTP = &net.UDPAddr{IP: remote.IP, Port: params.ClientPorts[0]}
		s.clientRTCP = &net.UDPAddr{IP: remote.IP, Port: params.ClientPorts[1]}
		headers["Transport"] = fmt.Sprintf(
			"RTP/AVP;unicast;client_port=%d-%d;server_port=%d-%d;ssrc=%08x",
			params.ClientPorts[0], params.ClientPorts[1], serverPorts[0], serverPorts[1], ssrc)
	}
	s.transportReady = true
	s.writeResponse(req.cseq, "200 OK", headers, nil)
	return true
}

func audittransport(value string) (auditrtsp.TransportParams, error) {
	if value == "" {
		return auditrtsp.TransportParams{}, fmt.Errorf("missing Transport header")
	}
	return auditrtsp.ParseTransportParams(value)
}

func bindServerUDPPair() (*net.UDPConn, *net.UDPConn, [2]int, error) {
	ip := net.IPv4(127, 0, 0, 1)
	var ports [2]int
	for attempt := 0; attempt < 100; attempt++ {
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err != nil {
			return nil, nil, ports, err
		}
		port := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port + 1})
		if err == nil {
			ports = [2]int{port, port + 1}
			return rtpConn, rtcpConn, ports, nil
		}
		rtpConn.Close()
	}
	return nil, nil, ports, fmt.Errorf("could not bind consecutive server UDP ports")
}

func (s *session) play(req request) bool {
	if !s.transportReady || (s.server.cfg.Policy == "strict" && !s.seenDESCRIBE) {
		s.writeResponse(req.cseq, "455 Method Not Valid in This State", nil, nil)
		return true
	}
	s.streamMu.Lock()
	if s.complete {
		s.nextPacket, s.complete, s.stalled = 0, false, false
	}
	start := s.nextPacket
	s.streamMu.Unlock()
	if len(s.server.packets) == 0 {
		s.writeResponse(req.cseq, "500 Internal Server Error", nil, nil)
		return true
	}
	if start >= len(s.server.packets) {
		start = 0
	}
	first := s.server.packets[start].RTP
	headers := s.sessionHeaders()
	headers["RTP-Info"] = fmt.Sprintf("%s/trackID=0;seq=%d;rtptime=%d",
		strings.TrimSuffix(req.url, "/trackID=0"), first.SequenceNumber, first.Timestamp)
	if err := s.writeResponse(req.cseq, "200 OK", headers, nil); err != nil {
		return false
	}
	s.startStream(start)
	return true
}

func (s *session) startStream(start int) {
	s.streamMu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	end := len(s.server.packets)
	stall := false
	if n := s.server.cfg.StallAfter; n > 0 && !s.stalled && start < n && n < end {
		end = n
		stall = true
		s.stalled = true
	}
	ctx, cancel := context.WithCancel(s.server.ctx)
	s.cancel = cancel
	s.streamMu.Unlock()

	s.server.wg.Add(1)
	go func() {
		defer s.server.wg.Done()
		s.sendPackets(ctx, start, end, stall)
	}()
}

func (s *session) sendPackets(ctx context.Context, start, end int, stall bool) {
	first := s.server.packets[start].RTP
	_ = s.sendRTCP(&rtcp.SenderReport{
		SSRC: ssrc, RTPTime: first.Timestamp,
		PacketCount: uint32(end - start), OctetCount: 1,
	})
	for i := start; i < end; i++ {
		if ctx.Err() != nil {
			return
		}
		data, err := s.server.packets[i].RTP.Marshal()
		if err != nil {
			return
		}
		if err := s.sendRTP(data); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Microsecond):
		}
	}
	s.streamMu.Lock()
	s.nextPacket = end
	if !stall {
		s.complete = true
	}
	s.streamMu.Unlock()
	if stall || ctx.Err() != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Millisecond):
	}
	bye, err := rtcp.Goodbye{Sources: []uint32{ssrc}, Reason: "fixture complete"}.Marshal()
	if err == nil {
		_ = s.sendRTCPBytes(bye)
	}
}

func (s *session) sendRTP(data []byte) error {
	if s.tcpMode {
		return s.writeInterleaved(0, data)
	}
	if s.rtpConn == nil || s.clientRTP == nil {
		return errors.New("udp session is not set up")
	}
	_, err := s.rtpConn.WriteToUDP(data, s.clientRTP)
	return err
}

func (s *session) sendRTCP(packet rtcp.Packet) error {
	data, err := packet.Marshal()
	if err != nil {
		return err
	}
	return s.sendRTCPBytes(data)
}

func (s *session) sendRTCPBytes(data []byte) error {
	if s.tcpMode {
		return s.writeInterleaved(1, data)
	}
	if s.rtcpConn == nil || s.clientRTCP == nil {
		return errors.New("udp RTCP session is not set up")
	}
	_, err := s.rtcpConn.WriteToUDP(data, s.clientRTCP)
	return err
}

func (s *session) writeInterleaved(channel byte, payload []byte) error {
	if len(payload) > 65535 {
		return fmt.Errorf("interleaved payload too large: %d", len(payload))
	}
	header := []byte{'$', channel, byte(len(payload) >> 8), byte(len(payload))}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer s.conn.SetWriteDeadline(time.Time{})
	if _, err := s.conn.Write(header); err != nil {
		return err
	}
	_, err := s.conn.Write(payload)
	return err
}
