package relay

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/signal"
)

type ingress struct {
	*basePeer
	engine   *Engine
	room     string
	endpoint string
	started  bool
	startAt  time.Time
	startRTP uint32
}

func newIngress(engine *Engine, room, endpoint string) (*ingress, error) {
	pc, err := engine.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("new ingress peer connection: %w", err)
	}
	peer := &ingress{basePeer: newBasePeer(pc), engine: engine, room: room, endpoint: endpoint}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go peer.readTrack(track)
	})
	return peer, nil
}

func (p *ingress) readTrack(track *webrtc.TrackRemote) {
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if !p.started {
			p.started = true
			p.startAt = timeNow()
			p.startRTP = packet.Timestamp
		}
		elapsed := time.Duration(uint32(packet.Timestamp-p.startRTP)) * time.Second / 48000
		p.engine.broadcast(packet, p.startAt.Add(elapsed))
	}
}

// CreateOffer is invalid on the ingress: the publisher always offers.
func (p *ingress) CreateOffer() (string, error) { return "", notSupported("CreateOffer") }

// AcceptOffer creates the relay-side answer.
func (p *ingress) AcceptOffer(offer string) (string, error) {
	if err := guard.ValidateSDP(offer); err != nil {
		return "", err
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", fmt.Errorf("set ingress offer: %w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create ingress answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set ingress answer: %w", err)
	}
	return answer.SDP, nil
}

// AcceptAnswer is invalid on the ingress.
func (p *ingress) AcceptAnswer(string) error { return notSupported("AcceptAnswer") }

var _ signal.Peer = (*ingress)(nil)
