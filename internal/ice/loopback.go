package ice

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type Candidate struct {
	Address    string `json:"address"`
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	Type       string `json:"type,omitempty"`
	Foundation string `json:"foundation,omitempty"`
}

func (c Candidate) String() string {
	return net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
}

func (c Candidate) Family() string {
	ip := net.ParseIP(c.Address)
	if ip == nil {
		return "unknown"
	}
	if ip.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

type Pair struct {
	Local  Candidate `json:"local"`
	Remote Candidate `json:"remote"`
}

func IsLoopback(address string) bool {
	ip := net.ParseIP(strings.Trim(address, "[]"))
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 127
	}
	return ip.Equal(net.IPv6loopback)
}

func Validate(c Candidate) error {
	if c.Address == "" {
		return fmt.Errorf("candidate address is empty")
	}
	if !IsLoopback(c.Address) {
		return fmt.Errorf("candidate %s is not a loopback address", c.Address)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("candidate port %d is invalid", c.Port)
	}
	return nil
}

func Parse(address string) (Candidate, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return Candidate{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Candidate{}, err
	}
	candidate := Candidate{Address: host, Port: port, Protocol: "udp"}
	return candidate, Validate(candidate)
}

type FilterResult struct {
	Gathered int         `json:"gathered"`
	Accepted []Candidate `json:"accepted_candidates"`
	Rejected []string    `json:"rejected_candidates"`
}

func FilterCandidates(candidates []Candidate) FilterResult {
	result := FilterResult{Gathered: len(candidates)}
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		if err := Validate(candidate); err != nil {
			result.Rejected = append(result.Rejected, candidate.String())
			continue
		}
		key := candidate.Family() + "|" + candidate.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result.Accepted = append(result.Accepted, candidate)
	}
	return result
}

// EnumeratePairs returns the cross-product of accepted loopback candidates.
// Same-family pairs are ordered first, matching the transport families that
// the ICE agent can actually probe.
func EnumeratePairs(local, remote []Candidate) []Pair {
	pairs := make([]Pair, 0, len(local)*len(remote))
	for _, left := range local {
		for _, right := range remote {
			if left.Family() == right.Family() {
				pairs = append(pairs, Pair{Local: left, Remote: right})
			}
		}
	}
	for _, left := range local {
		for _, right := range remote {
			if left.Family() != right.Family() {
				pairs = append(pairs, Pair{Local: left, Remote: right})
			}
		}
	}
	return pairs
}
