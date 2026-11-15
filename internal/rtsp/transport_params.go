package rtsp

import (
	"fmt"
	"strconv"
	"strings"
)

// TransportParams is the parsed subset of an RTSP Transport header.
type TransportParams struct {
	Protocol    string
	Unicast     bool
	ClientPorts [2]int
	ServerPorts [2]int
	Interleaved [2]int
	SSRC        uint32
}

// ParseTransportParams parses semicolon-separated RTSP transport parameters.
func ParseTransportParams(value string) (TransportParams, error) {
	var out TransportParams
	parts := strings.Split(value, ";")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return out, fmt.Errorf("empty Transport header")
	}
	out.Protocol = strings.TrimSpace(parts[0])
	for _, part := range parts[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			if strings.EqualFold(strings.TrimSpace(part), "unicast") {
				out.Unicast = true
			}
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "client_port":
			pair, err := parsePortPair(val)
			if err != nil {
				return out, fmt.Errorf("client_port: %w", err)
			}
			out.ClientPorts = pair
		case "server_port":
			pair, err := parsePortPair(val)
			if err != nil {
				return out, fmt.Errorf("server_port: %w", err)
			}
			out.ServerPorts = pair
		case "interleaved":
			pair, err := parseChannelPair(val)
			if err != nil {
				return out, fmt.Errorf("interleaved: %w", err)
			}
			out.Interleaved = pair
		case "ssrc":
			n, err := strconv.ParseUint(strings.TrimPrefix(val, "0x"), 16, 32)
			if err != nil {
				return out, fmt.Errorf("ssrc: %w", err)
			}
			out.SSRC = uint32(n)
		}
	}
	return out, nil
}

func parsePortPair(value string) ([2]int, error) {
	var pair [2]int
	left, right, ok := strings.Cut(value, "-")
	if !ok {
		return pair, fmt.Errorf("expected pair, got %q", value)
	}
	for i, text := range []string{left, right} {
		n, err := strconv.Atoi(text)
		if err != nil || n <= 0 || n > 65535 {
			return pair, fmt.Errorf("invalid port %q", text)
		}
		pair[i] = n
	}
	if pair[1] != pair[0]+1 {
		return pair, fmt.Errorf("ports are not consecutive: %d-%d", pair[0], pair[1])
	}
	return pair, nil
}

func parseChannelPair(value string) ([2]int, error) {
	var pair [2]int
	left, right, ok := strings.Cut(value, "-")
	if !ok {
		return pair, fmt.Errorf("expected pair, got %q", value)
	}
	for i, text := range []string{left, right} {
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 || n > 255 {
			return pair, fmt.Errorf("invalid channel %q", text)
		}
		pair[i] = n
	}
	if pair[1] != pair[0]+1 {
		return pair, fmt.Errorf("channels are not consecutive: %d-%d", pair[0], pair[1])
	}
	return pair, nil
}
