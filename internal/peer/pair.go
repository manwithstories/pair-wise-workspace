package peer

import (
	"fmt"
	"net"

	"github.com/pion/webrtc/v4"
)

// recordSelectedPair inspects the selected candidate pair, verifies both ends
// are loopback and stores the addresses for the metrics report.
func (p *Pair) recordSelectedPair() error {
	senders := p.Sender.GetSenders()
	if len(senders) == 0 || senders[0].Transport() == nil {
		return nil
	}
	pair, err := senders[0].Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil {
		return fmt.Errorf("selected candidate pair: %w", err)
	}
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return nil
	}
	for _, c := range []*webrtc.ICECandidate{pair.Local, pair.Remote} {
		ip := net.ParseIP(c.Address)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("%w: %s", ErrNonLoopbackCandidate, c.Address)
		}
	}
	p.mu.Lock()
	p.localAddr = net.JoinHostPort(pair.Local.Address, fmt.Sprint(pair.Local.Port))
	p.remoteAddr = net.JoinHostPort(pair.Remote.Address, fmt.Sprint(pair.Remote.Port))
	p.mu.Unlock()
	return nil
}

// SelectedPair reports the local and remote addresses of the selected ICE
// pair. Both are empty before the connection is established.
func (p *Pair) SelectedPair() (local, remote string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.localAddr, p.remoteAddr
}

// Candidates returns the candidates gathered so far.
func (p *Pair) Candidates() []Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Candidate, len(p.candidates))
	copy(out, p.candidates)
	return out
}

// Rejected returns the human-readable reasons candidates were refused.
func (p *Pair) Rejected() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.rejected))
	copy(out, p.rejected)
	return out
}

// AllLoopback reports whether every gathered candidate has a loopback
// address. It is true for an empty set, which callers should treat as "not
// yet known" rather than as proof of containment.
func (p *Pair) AllLoopback() bool {
	for _, c := range p.Candidates() {
		ip := net.ParseIP(c.Address)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}
