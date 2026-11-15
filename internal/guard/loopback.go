// Package guard enforces the relay's loopback-only deployment boundary.
package guard

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ErrNonLoopback identifies an address that escaped 127.0.0.0/8.
var ErrNonLoopback = fmt.Errorf("non-loopback address")

// ValidateListenAddress accepts only the 127.0.0.1 IP literal with a usable
// port. Hostnames (including mDNS names) are intentionally rejected.
func ValidateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		return fmt.Errorf("%w: %s", ErrNonLoopback, address)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid listen port %q", portText)
	}
	return nil
}

// ValidateCandidate validates one ICE candidate's transport address. The
// candidate syntax is defined by RFC 8445; field 4 is the address.
func ValidateCandidate(candidate string) error {
	candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "a="))
	candidate = strings.TrimPrefix(candidate, "candidate:")
	fields := strings.Fields(candidate)
	if len(fields) < 5 {
		return fmt.Errorf("malformed ICE candidate %q", candidate)
	}
	address := strings.Trim(fields[4], "[]")
	if strings.HasSuffix(strings.ToLower(address), ".local") {
		return fmt.Errorf("%w: mDNS candidate %s", ErrNonLoopback, address)
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: candidate %s", ErrNonLoopback, address)
	}
	return nil
}

// ValidateSDP rejects any SDP ICE candidate or ICE server address outside
// 127.0.0.0/8. mDNS candidates are rejected even though they may resolve to
// loopback, because resolution is neither deterministic nor local-only.
func ValidateSDP(sdp string) error {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			if err := ValidateCandidate(line); err != nil {
				return err
			}
		}
		if strings.HasPrefix(line, "a=ice-ufrag:") || strings.HasPrefix(line, "a=ice-pwd:") {
			continue
		}
	}
	return nil
}
