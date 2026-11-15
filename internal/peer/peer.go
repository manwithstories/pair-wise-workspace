package peer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	loopice "rtprelay/internal/ice"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

type Config struct {
	ConnectTimeout time.Duration
}

type Peer struct {
	PC    *webrtc.PeerConnection
	Track *webrtc.TrackLocalStaticRTP

	mu          sync.Mutex
	candidates  []loopice.Candidate
	rejected    []string
	gathered    int
	onCandidate func(*webrtc.ICECandidate)
	gatherDone  chan struct{}
	gatherOnce  sync.Once
	remoteTrack chan *webrtc.TrackRemote
	trackOnce   sync.Once
}

func NewLoopback(cfg Config) (*Peer, error) {
	var settingEngine webrtc.SettingEngine
	settingEngine.SetNetworkTypes([]webrtc.NetworkType{
		webrtc.NetworkTypeUDP4,
		webrtc.NetworkTypeUDP6,
	})
	settingEngine.SetIPFilter(func(ip net.IP) bool {
		return loopice.IsLoopback(ip.String())
	})
	settingEngine.SetIncludeLoopbackCandidate(true)
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settingEngine.SetFireOnTrackBeforeFirstRTP(true)
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	settingEngine.SetICETimeouts(cfg.ConnectTimeout/3, cfg.ConnectTimeout, 2*time.Second)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	p := &Peer{
		PC:          pc,
		gatherDone:  make(chan struct{}),
		remoteTrack: make(chan *webrtc.TrackRemote, 1),
	}
	pc.OnICECandidate(p.handleCandidate)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		p.trackOnce.Do(func() { p.remoteTrack <- track })
	})
	return p, nil
}

func (p *Peer) AddAudioTrack(ssrc uint32) error {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"commentary", "rtprelay",
	)
	if err != nil {
		return err
	}
	if _, err := p.PC.AddTrack(track); err != nil {
		return err
	}
	p.Track = track
	return nil
}

func (p *Peer) SetCandidateHandler(handler func(*webrtc.ICECandidate)) {
	p.mu.Lock()
	p.onCandidate = handler
	p.mu.Unlock()
}

func (p *Peer) handleCandidate(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		p.gatherOnce.Do(func() { close(p.gatherDone) })
		p.mu.Lock()
		handler := p.onCandidate
		p.mu.Unlock()
		if handler != nil {
			handler(nil)
		}
		return
	}
	raw := loopice.Candidate{
		Address: candidate.Address, Port: int(candidate.Port),
		Protocol: candidate.Protocol.String(), Type: candidate.Typ.String(),
		Foundation: candidate.Foundation,
	}
	p.mu.Lock()
	p.gathered++
	if err := loopice.Validate(raw); err != nil {
		p.rejected = append(p.rejected, raw.String())
		p.mu.Unlock()
		return
	}
	p.candidates = append(p.candidates, raw)
	handler := p.onCandidate
	p.mu.Unlock()
	if handler != nil {
		cloned := *candidate
		handler(&cloned)
	}
}

func (p *Peer) WaitGathering(ctx context.Context) error {
	select {
	case <-p.gatherDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ICE gathering: %w", ctx.Err())
	}
}

func (p *Peer) WaitConnected(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch p.PC.ICEConnectionState() {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			return nil
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			return fmt.Errorf("ICE connection state %s", p.PC.ICEConnectionState())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ICE connect: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (p *Peer) WaitRemoteTrack(ctx context.Context) (*webrtc.TrackRemote, error) {
	select {
	case track := <-p.remoteTrack:
		return track, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait remote track: %w", ctx.Err())
	}
}

type GatherReport struct {
	Gathered int                 `json:"gathered"`
	Accepted []loopice.Candidate `json:"accepted"`
	Rejected []string            `json:"rejected"`
}

func (p *Peer) GatherReport() GatherReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	filtered := loopice.FilterCandidates(append([]loopice.Candidate(nil), p.candidates...))
	rejected := append([]string(nil), p.rejected...)
	rejected = append(rejected, filtered.Rejected...)
	return GatherReport{
		Gathered: p.gathered, Accepted: filtered.Accepted, Rejected: rejected,
	}
}

func (p *Peer) SelectedPair() (loopice.Pair, error) {
	if p.PC.SCTP() == nil || p.PC.SCTP().Transport() == nil {
		return loopice.Pair{}, fmt.Errorf("SCTP transport unavailable")
	}
	pair, err := p.PC.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil {
		return loopice.Pair{}, err
	}
	if pair == nil {
		return loopice.Pair{}, fmt.Errorf("no selected ICE candidate pair")
	}
	return loopice.Pair{
		Local: loopice.Candidate{
			Address: pair.Local.Address, Port: int(pair.Local.Port), Protocol: pair.Local.Protocol.String(),
		},
		Remote: loopice.Candidate{
			Address: pair.Remote.Address, Port: int(pair.Remote.Port), Protocol: pair.Remote.Protocol.String(),
		},
	}, nil
}

func (p *Peer) Close() error {
	if p.PC == nil {
		return nil
	}
	return p.PC.Close()
}
