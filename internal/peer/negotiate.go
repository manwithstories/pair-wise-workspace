package peer

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

// Negotiate applies the offer/answer exchange directly between the two
// in-process peers and waits for the connection. SDP relayed through the
// signaling server uses ApplyRemoteOffer/ApplyAnswer instead.
func (p *Pair) Negotiate() error {
	offer, err := p.CreateOffer()
	if err != nil {
		return err
	}
	if err := p.ApplyRemoteOffer(offer); err != nil {
		return err
	}
	answer, err := p.CreateAnswer()
	if err != nil {
		return err
	}
	if err := p.ApplyAnswer(answer); err != nil {
		return err
	}
	return p.WaitConnected(DefaultConnectTimeout)
}

// CreateOffer builds and installs the sender's local offer.
func (p *Pair) CreateOffer() (webrtc.SessionDescription, error) {
	// CreateOffer is called once per pair; a second call would re-apply Send
	// and fail, so guard on the already-configured flag.
	if p.ssrc != 0 && !p.ssrcApplied.Load() {
		if err := p.SetSSRC(p.ssrc); err != nil {
			return webrtc.SessionDescription{}, err
		}
		p.ssrcApplied.Store(true)
	}
	offer, err := p.Sender.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create offer: %w", err)
	}
	if err := p.Sender.SetLocalDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("sender set local: %w", err)
	}
	return offer, nil
}

// ApplyRemoteOffer installs an offer on the receiver.
func (p *Pair) ApplyRemoteOffer(offer webrtc.SessionDescription) error {
	if err := p.Receiver.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("receiver set remote: %w", err)
	}
	return nil
}

// CreateAnswer builds and installs the receiver's local answer.
func (p *Pair) CreateAnswer() (webrtc.SessionDescription, error) {
	answer, err := p.Receiver.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create answer: %w", err)
	}
	if err := p.Receiver.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("receiver set local: %w", err)
	}
	return answer, nil
}

// ApplyAnswer installs the answer on the sender.
func (p *Pair) ApplyAnswer(answer webrtc.SessionDescription) error {
	if err := p.Sender.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("sender set remote: %w", err)
	}
	return nil
}

// LocalCandidates returns every gathered candidate for a side, in the form
// the signaling protocol carries.
func (p *Pair) LocalCandidates(side string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.candidates {
		if c.Side == side && c.Raw != "" {
			out = append(out, c.Raw)
		}
	}
	return out
}

// AddRemoteCandidate applies a candidate that was produced by fromSide.
//
// A candidate belongs on the PC whose remote description already names its
// sender: the offerer's candidates carry the offer's ice-ufrag, which the
// receiver installed, so they go to the Receiver PC; the answerer's
// candidates carry the answer's ufrag and go to the Sender PC. Pion silently
// drops a candidate whose ufrag does not match, so getting this backwards
// looks exactly like a network failure.
func (p *Pair) AddRemoteCandidate(fromSide, raw string) error {
	pc := p.Receiver
	if fromSide == "receiver" {
		pc = p.Sender
	}
	if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: raw}); err != nil {
		return fmt.Errorf("add remote candidate from %s: %w", fromSide, err)
	}
	return nil
}

// WaitConnected blocks until ICE and DTLS are both up, or the timeout expires.
// DTLS failure is distinguished from an ICE timeout so the CLI can pick the
// right exit code.
func (p *Pair) WaitConnected(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		switch state := p.Sender.ConnectionState(); state {
		case webrtc.PeerConnectionStateConnected:
			return p.recordSelectedPair()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if p.Sender.ICEConnectionState() == webrtc.ICEConnectionStateFailed ||
				p.Sender.ICEConnectionState() == webrtc.ICEConnectionStateDisconnected {
				return fmt.Errorf("%w: state %s", ErrICETimeout, state)
			}
			return fmt.Errorf("%w: state %s", ErrDTLSFailed, state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s (state %s, ice %s)", ErrICETimeout, timeout,
				p.Sender.ConnectionState(), p.Sender.ICEConnectionState())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
