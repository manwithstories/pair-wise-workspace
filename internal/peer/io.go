package peer

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Track waits for the inbound track to be created by the remote peer.
func (p *Pair) TrackRemote(timeout time.Duration) (*webrtc.TrackRemote, error) {
	if timeout <= 0 {
		timeout = DefaultMediaTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		for _, r := range p.Receiver.GetReceivers() {
			if t := r.Track(); t != nil {
				return t, nil
			}
		}
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: no remote track after %s", ErrDTLSFailed, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SetSSRC pins the outbound SSRC in the negotiated send parameters, so the
// fixture's fixed SSRC reaches the wire instead of Pion's random assignment.
// It must be called after the local description is set (Pion fills the send
// parameters then) and before the first packet is written (Send may only be
// called once, and startRTPSenders calls it during negotiation).
func (p *Pair) SetSSRC(ssrc uint32) error {
	senders := p.Sender.GetSenders()
	if len(senders) == 0 {
		return fmt.Errorf("no RTP sender to configure")
	}
	params := senders[0].GetParameters()
	if len(params.Encodings) == 0 {
		return fmt.Errorf("sender has no encodings")
	}
	params.Encodings[0].SSRC = webrtc.SSRC(ssrc)
	if err := senders[0].Send(params); err != nil {
		return fmt.Errorf("apply SSRC %d: %w", ssrc, err)
	}
	return nil
}

// ReadLoop delivers every received RTP packet to onPacket until the track
// closes or the pair is closed. It blocks, so callers run it in a goroutine.
func (p *Pair) ReadLoop(timeout time.Duration, onPacket func(*rtp.Packet) error) error {
	track, err := p.TrackRemote(timeout)
	if err != nil {
		return err
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return ErrClosed
			}
			return fmt.Errorf("read RTP: %w", err)
		}
		if err := onPacket(pkt); err != nil {
			return err
		}
	}
}

// WritePacket sends one already-marshalled RTP packet on the outbound track.
// Pion rewrites the SSRC to the negotiated value, so callers should read the
// SSRC from the receiver rather than assuming the fixture's.
func (p *Pair) WritePacket(raw []byte) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if _, err := p.Track.Write(raw); err != nil {
		return fmt.Errorf("write RTP: %w", err)
	}
	return nil
}

// WriteRTP serializes and sends one packet.
func (p *Pair) WriteRTP(pkt *rtp.Packet) error {
	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("marshal RTP: %w", err)
	}
	return p.WritePacket(raw)
}
