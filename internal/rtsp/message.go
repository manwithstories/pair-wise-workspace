package rtsp

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/sdp"
)

var (
	// ErrProtocol marks malformed or rejected RTSP/RTP exchanges.
	ErrProtocol = errors.New("rtsp protocol error")
	// ErrSessionTimeout marks an RTSP 454 response.
	ErrSessionTimeout = errors.New("rtsp session timeout")
	// ErrSilenceExhausted means the UDP reconnect budget was exhausted.
	ErrSilenceExhausted = errors.New("udp silence reconnect limit exceeded")
)

// Response is a parsed RTSP response.
type Response struct {
	StatusCode int
	Status     string
	Headers    map[string]string
	Body       []byte
}

// ParseResponse decodes one complete RTSP response.
func ParseResponse(data []byte) (Response, error) {
	var out Response
	split := strings.Index(string(data), "\r\n\r\n")
	if split < 0 {
		return out, fmt.Errorf("%w: missing response header terminator", ErrProtocol)
	}
	lines := strings.Split(string(data[:split]), "\r\n")
	status := strings.Fields(lines[0])
	if len(status) < 2 || status[0] != "RTSP/1.0" {
		return out, fmt.Errorf("%w: invalid status line %q", ErrProtocol, lines[0])
	}
	code, err := strconv.Atoi(status[1])
	if err != nil {
		return out, fmt.Errorf("%w: invalid status code %q", ErrProtocol, status[1])
	}
	out.StatusCode = code
	out.Status = strings.Join(status[2:], " ")
	out.Headers = make(map[string]string)
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return out, fmt.Errorf("%w: invalid header %q", ErrProtocol, line)
		}
		out.Headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	out.Body = append([]byte(nil), data[split+4:]...)
	return out, nil
}

func buildRequest(method, rawURL, cseq string, headers map[string]string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %s\r\nUser-Agent: rtspaudit\r\n", method, rawURL, cseq)
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", key, headers[key])
	}
	b.WriteString("Content-Length: 0\r\n\r\n")
	return []byte(b.String())
}

func sessionID(header string) string {
	id, _, _ := strings.Cut(header, ";")
	return strings.TrimSpace(id)
}

func rtpInfoSeq(header string) (uint16, bool) {
	for _, part := range strings.Split(header, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || key != "seq" {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return 0, false
		}
		return uint16(n), true
	}
	return 0, false
}

func validateSDP(body []byte) error {
	var raw sdp.SessionDescription
	if err := raw.Unmarshal(body); err != nil {
		return fmt.Errorf("%w: invalid SDP: %v", ErrProtocol, err)
	}
	var desc description.Session
	if err := desc.Unmarshal(&raw); err != nil {
		return fmt.Errorf("%w: unsupported SDP: %v", ErrProtocol, err)
	}
	if len(desc.Medias) == 0 {
		return fmt.Errorf("%w: SDP has no media", ErrProtocol)
	}
	return nil
}
