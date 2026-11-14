package mocksrv

import (
	"fmt"
	"net"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

// handleSetup negotiates one track. It returns false when the connection
// must be closed.
func (s *Server) handleSetup(nc net.Conn, req *base.Request, st *connState, write func(*base.Response)) bool {
	if st.id == "" {
		st.id = fmt.Sprintf("sess-%d", s.sessN.Add(1))
	}
	tp := parseTransportHeader(req.Header["Transport"][0])
	trackIdx := len(st.tracks)
	if _, tcp := tp["rtp/avp/tcp"]; tcp {
		ch := 2 * trackIdx
		st.tracks[trackIdx] = &trackSetup{interleaved: true, channel: ch}
		write(ok(req, base.Header{
			"Session": base.HeaderValue{st.id + ";timeout=60"},
			"Transport": base.HeaderValue{fmt.Sprintf(
				"RTP/AVP/TCP;unicast;interleaved=%d-%d", ch, ch+1)},
		}))
		return true
	}
	// UDP unicast: bind a send socket and advertise its port pair.
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		write(&base.Response{StatusCode: base.StatusInternalServerError, Header: cseqOf(req)})
		return true
	}
	var clientPort int
	fmt.Sscanf(tp["client_port"], "%d", &clientPort)
	st.tracks[trackIdx] = &trackSetup{
		udpDst:  &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: clientPort},
		udpConn: uc,
	}
	serverPort := uc.LocalAddr().(*net.UDPAddr).Port
	write(ok(req, base.Header{
		"Session": base.HeaderValue{st.id + ";timeout=60"},
		"Transport": base.HeaderValue{fmt.Sprintf(
			"RTP/AVP/UDP;unicast;client_port=%s;server_port=%d-%d",
			tp["client_port"], serverPort, serverPort+1)},
	}))
	return true
}

// handlePlay acknowledges PLAY and starts the streamer.
func (s *Server) handlePlay(nc net.Conn, req *base.Request, st *connState, write func(*base.Response)) {
	write(ok(req, base.Header{"Session": base.HeaderValue{st.id}}))
	s.stopStream(st)
	st.stream = newStreamer(s.cfg, st, nc)
	st.stream.start()
}
