package runner

import (
	"fmt"
	"net"
	"strings"

	loopice "rtprelay/internal/ice"
)

type loopbackProbe struct {
	conn      *net.UDPConn
	candidate loopice.Candidate
	sdpLine   string
}

func newLoopbackProbe() (*loopbackProbe, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	candidate := loopice.Candidate{Address: "127.0.0.1", Port: port, Protocol: "udp", Type: "host"}
	return &loopbackProbe{
		conn: conn, candidate: candidate,
		sdpLine: fmt.Sprintf("a=candidate:probe%d 1 udp 1000000000 127.0.0.1 %d typ host generation 0", port, port),
	}, nil
}

func (p *loopbackProbe) Close() error {
	return p.conn.Close()
}

func addProbeCandidate(sdp, candidateLine string) string {
	line := candidateLine + "\r\n"
	if index := strings.Index(sdp, "a=end-of-candidates"); index >= 0 {
		return sdp[:index] + line + sdp[index:]
	}
	return strings.TrimRight(sdp, "\r\n") + "\r\n" + line
}
