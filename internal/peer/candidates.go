package peer

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"relayctl/internal/signal"
)

// ErrNonLoopbackCandidate is returned when a candidate violates relayctl's
// local-only policy.
var ErrNonLoopbackCandidate = errors.New("peer: non-loopback candidate")

// ParsedCandidate is the small, stable view of an ICE candidate used in reports.
type ParsedCandidate struct {
	Raw      string `json:"raw"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
}

// Address returns host:port/protocol for terminal output.
func (c ParsedCandidate) Address() string {
	return fmt.Sprintf("%s:%d/%s", c.Host, c.Port, strings.ToLower(c.Protocol))
}

// CandidatePair is one cross product of a local and remote candidate.
type CandidatePair struct {
	Local  ParsedCandidate `json:"local"`
	Remote ParsedCandidate `json:"remote"`
}

// SelectedPair records the ICE pair ultimately chosen by the agent.
type SelectedPair struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// ParseCandidate validates the loopback whitelist and extracts report fields.
func ParseCandidate(raw string) (ParsedCandidate, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ParsedCandidate{}, fmt.Errorf("%w: empty candidate", signal.ErrInvalidMessage)
	}
	cand, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		return ParsedCandidate{}, fmt.Errorf("%w: %v", signal.ErrInvalidMessage, err)
	}
	ip := net.ParseIP(cand.Address())
	if ip == nil || !ip.IsLoopback() {
		return ParsedCandidate{}, fmt.Errorf("%w: %s", ErrNonLoopbackCandidate, cand.Address())
	}
	if cand.Port() <= 0 || cand.Port() > 65535 {
		return ParsedCandidate{}, fmt.Errorf("%w: bad port", signal.ErrInvalidMessage)
	}
	return ParsedCandidate{
		Raw:      raw,
		Host:     cand.Address(),
		Port:     cand.Port(),
		Protocol: strings.ToUpper(cand.NetworkType().String()),
		Type:     cand.Type().String(),
	}, nil
}

// CandidateSet tracks unique local/remote candidates and rejected candidates.
type CandidateSet struct {
	mu     sync.Mutex
	local  map[string]ParsedCandidate
	remote map[string]ParsedCandidate
	banned uint64
}

// NewCandidateSet returns an empty candidate set.
func NewCandidateSet() *CandidateSet {
	return &CandidateSet{
		local:  make(map[string]ParsedCandidate),
		remote: make(map[string]ParsedCandidate),
	}
}

func candidateKey(c ParsedCandidate) string {
	return c.Address() + "/" + c.Type
}

// AddLocal records a locally gathered candidate. Local candidates are also
// checked because a misconfigured interface filter is a hard policy failure.
func (s *CandidateSet) AddLocal(raw string) (ParsedCandidate, error) {
	c, err := ParseCandidate(raw)
	if err != nil {
		if errors.Is(err, ErrNonLoopbackCandidate) {
			s.mu.Lock()
			s.banned++
			s.mu.Unlock()
		}
		return ParsedCandidate{}, err
	}
	s.mu.Lock()
	s.local[candidateKey(c)] = c
	s.mu.Unlock()
	return c, nil
}

// AddRemote records a peer candidate and enforces the loopback whitelist.
func (s *CandidateSet) AddRemote(raw string) (ParsedCandidate, error) {
	c, err := ParseCandidate(raw)
	if err != nil {
		if errors.Is(err, ErrNonLoopbackCandidate) {
			s.mu.Lock()
			s.banned++
			s.mu.Unlock()
		}
		return ParsedCandidate{}, err
	}
	s.mu.Lock()
	s.remote[candidateKey(c)] = c
	s.mu.Unlock()
	return c, nil
}

// Snapshot returns stable copies and counts under one lock.
func (s *CandidateSet) Snapshot() (local, remote []ParsedCandidate, banned uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.local {
		local = append(local, c)
	}
	for _, c := range s.remote {
		remote = append(remote, c)
	}
	return local, remote, s.banned
}

// EnumerateCandidatePairs produces the candidate-pair checklist cross product.
func EnumerateCandidatePairs(local, remote []ParsedCandidate) []CandidatePair {
	pairs := make([]CandidatePair, 0, len(local)*len(remote))
	for _, l := range local {
		for _, r := range remote {
			pairs = append(pairs, CandidatePair{Local: l, Remote: r})
		}
	}
	return pairs
}

// EnumerateRawPairs is a convenience for tests and probe callers holding raw
// candidates. Any policy-invalid candidate causes an error.
func EnumerateRawPairs(localRaw, remoteRaw []string) ([]CandidatePair, error) {
	local := make([]ParsedCandidate, 0, len(localRaw))
	for _, raw := range localRaw {
		c, err := ParseCandidate(raw)
		if err != nil {
			return nil, err
		}
		local = append(local, c)
	}
	remote := make([]ParsedCandidate, 0, len(remoteRaw))
	for _, raw := range remoteRaw {
		c, err := ParseCandidate(raw)
		if err != nil {
			return nil, err
		}
		remote = append(remote, c)
	}
	return EnumerateCandidatePairs(local, remote), nil
}

// SelectedPairFromPC reads the selected ICE pair from a negotiated media
// transport. Pion exposes that transport through an RTP sender or receiver.
func SelectedPairFromPC(pc *webrtc.PeerConnection) (SelectedPair, bool) {
	for _, sender := range pc.GetSenders() {
		if sender.Transport() == nil || sender.Transport().ICETransport() == nil {
			continue
		}
		pair, err := sender.Transport().ICETransport().GetSelectedCandidatePair()
		if err == nil && pair != nil && pair.Local != nil && pair.Remote != nil {
			return formatSelectedPair(pair), true
		}
	}
	for _, receiver := range pc.GetReceivers() {
		if receiver.Transport() == nil || receiver.Transport().ICETransport() == nil {
			continue
		}
		pair, err := receiver.Transport().ICETransport().GetSelectedCandidatePair()
		if err == nil && pair != nil && pair.Local != nil && pair.Remote != nil {
			return formatSelectedPair(pair), true
		}
	}
	return SelectedPair{}, false
}

func formatSelectedPair(pair *webrtc.ICECandidatePair) SelectedPair {
	return SelectedPair{
		Local:  candidateAddress(pair.Local),
		Remote: candidateAddress(pair.Remote),
	}
}

func candidateAddress(c *webrtc.ICECandidate) string {
	return fmt.Sprintf("%s:%d/%s", c.Address, c.Port, strings.ToLower(c.Protocol.String()))
}
