package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/signal"
)

const iceTimeout = 5 * time.Second

type mediaSession struct {
	client        *signal.Client
	pc            *webrtc.PeerConnection
	track         *webrtc.TrackLocalStaticRTP
	packets       atomic.Uint64
	errs          chan error
	ready         chan struct{}
	readyOnce     sync.Once
	closeOnce     sync.Once
	remoteMu      sync.Mutex
	remoteSet     bool
	pendingRemote []signal.Candidate
	localMu       sync.Mutex
	localEnabled  bool
	pendingLocal  []signal.Candidate
}

func newMediaSession(client *signal.Client, pc *webrtc.PeerConnection, track *webrtc.TrackLocalStaticRTP) *mediaSession {
	session := &mediaSession{client: client, pc: pc, track: track,
		errs: make(chan error, 4), ready: make(chan struct{})}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		session.addLocalCandidate(signal.Candidate{
			Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex,
		})
	})
	return session
}

func dialPublisher(ctx context.Context, api *webrtc.API, address, room string) (*mediaSession, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "audio", "publisher")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, err
	}
	client, _, err := signal.Dial(ctx, address, signal.RolePublisher, room)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	session := newMediaSession(client, pc, track)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		session.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		session.Close()
		return nil, err
	}
	if err := client.Send(signal.Message{Type: signal.TypeOffer, SDP: offer.SDP}); err != nil {
		session.Close()
		return nil, err
	}
	session.enableLocalCandidates()
	go session.pump(ctx)
	if err := session.waitRemote(ctx, iceTimeout); err != nil {
		session.Close()
		return nil, err
	}
	if err := waitConnected(ctx, pc, iceTimeout); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func dialSubscriber(ctx context.Context, api *webrtc.API, address, room string) (*mediaSession, error) {
	client, _, err := signal.Dial(ctx, address, signal.RoleSubscriber, room)
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	session := newMediaSession(client, pc, nil)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, err := track.ReadRTP(); err != nil {
					return
				}
				session.packets.Add(1)
			}
		}()
	})
	var offer string
	for offer == "" {
		message, err := client.Recv(ctx)
		if err != nil {
			session.Close()
			return nil, err
		}
		switch message.Type {
		case signal.TypeCandidate:
			if message.Candidate != nil {
				session.addRemoteCandidate(*message.Candidate)
			}
		case signal.TypeOffer:
			offer = message.SDP
		case signal.TypeBye:
			session.Close()
			return nil, io.EOF
		default:
			session.Close()
			return nil, fmt.Errorf("unexpected subscriber message %s", message.Type)
		}
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		session.Close()
		return nil, err
	}
	session.markRemote()
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		session.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		session.Close()
		return nil, err
	}
	if err := client.Send(signal.Message{Type: signal.TypeAnswer, SDP: answer.SDP}); err != nil {
		session.Close()
		return nil, err
	}
	session.enableLocalCandidates()
	go session.pump(ctx)
	if err := waitConnected(ctx, pc, iceTimeout); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func (s *mediaSession) pump(ctx context.Context) {
	for {
		message, err := s.client.Recv(ctx)
		if err != nil {
			s.report(err)
			return
		}
		switch message.Type {
		case signal.TypeCandidate:
			if message.Candidate != nil {
				s.addRemoteCandidate(*message.Candidate)
			}
		case signal.TypeAnswer:
			if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				s.report(err)
				return
			}
			s.markRemote()
		case signal.TypeBye:
			s.report(io.EOF)
			return
		default:
			s.report(fmt.Errorf("unexpected signaling message %s", message.Type))
			return
		}
	}
}

func (s *mediaSession) addLocalCandidate(candidate signal.Candidate) {
	s.localMu.Lock()
	if !s.localEnabled {
		s.pendingLocal = append(s.pendingLocal, candidate)
		s.localMu.Unlock()
		return
	}
	s.localMu.Unlock()
	s.sendCandidate(candidate)
}

func (s *mediaSession) enableLocalCandidates() {
	s.localMu.Lock()
	s.localEnabled = true
	pending := append([]signal.Candidate(nil), s.pendingLocal...)
	s.pendingLocal = nil
	s.localMu.Unlock()
	for _, candidate := range pending {
		s.sendCandidate(candidate)
	}
}

func (s *mediaSession) sendCandidate(candidate signal.Candidate) {
	if err := guard.ValidateCandidate(candidate.Candidate); err != nil {
		s.report(err)
		return
	}
	if err := s.client.Send(signal.Message{Type: signal.TypeCandidate, Candidate: &candidate}); err != nil {
		s.report(err)
	}
}

func (s *mediaSession) addRemoteCandidate(candidate signal.Candidate) {
	if err := guard.ValidateCandidate(candidate.Candidate); err != nil {
		s.report(err)
		return
	}
	s.remoteMu.Lock()
	if !s.remoteSet {
		s.pendingRemote = append(s.pendingRemote, candidate)
		s.remoteMu.Unlock()
		return
	}
	s.remoteMu.Unlock()
	if err := s.pc.AddICECandidate(toICE(candidate)); err != nil {
		s.report(err)
	}
}

func (s *mediaSession) markRemote() {
	s.remoteMu.Lock()
	s.remoteSet = true
	pending := append([]signal.Candidate(nil), s.pendingRemote...)
	s.pendingRemote = nil
	s.remoteMu.Unlock()
	for _, candidate := range pending {
		if err := s.pc.AddICECandidate(toICE(candidate)); err != nil {
			s.report(err)
		}
	}
	s.readyOnce.Do(func() { close(s.ready) })
}

func toICE(candidate signal.Candidate) webrtc.ICECandidateInit {
	return webrtc.ICECandidateInit{Candidate: candidate.Candidate,
		SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex}
}

func (s *mediaSession) waitRemote(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		return nil
	case err := <-s.errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("SDP handshake timeout")
	}
}

func waitConnected(ctx context.Context, pc *webrtc.PeerConnection, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return nil
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			return fmt.Errorf("peer connection state %s", pc.ConnectionState())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ICE connection timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *mediaSession) WriteRTP(packet *rtp.Packet) error {
	if s.track == nil {
		return fmt.Errorf("session has no local track")
	}
	return s.track.WriteRTP(packet)
}

func (s *mediaSession) report(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func (s *mediaSession) Close() error {
	var first error
	s.closeOnce.Do(func() {
		if s.client != nil {
			first = s.client.Close()
		}
		if s.pc != nil {
			if err := s.pc.Close(); err != nil && first == nil {
				first = err
			}
		}
	})
	return first
}
