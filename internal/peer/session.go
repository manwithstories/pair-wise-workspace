// Package peer wraps Pion PeerConnection setup, loopback-only ICE gathering,
// candidate accounting, and media lifecycle for relayctl.
package peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/transport/v3/stdnet"
	"github.com/pion/webrtc/v4"

	"relayctl/internal/signal"
)

var (
	ErrICEFailed         = errors.New("peer: ICE failed")
	ErrConnectionTimeout = errors.New("peer: connection timeout")
)

// Session owns one PeerConnection and all of its media/ICE state.
type Session struct {
	role string
	pc   *webrtc.PeerConnection
	mux  *ice.MultiUDPMuxDefault

	senderTrack *webrtc.TrackLocalStaticRTP
	remoteTrack chan *webrtc.TrackRemote
	onCandidate func(signal.ICECandidate)
	candidates  *CandidateSet

	remoteMu   sync.Mutex
	remoteSet  bool
	queuedCand []signal.ICECandidate

	connected     chan struct{}
	connectedOnce sync.Once
	gathered      chan struct{}
	gatheredOnce  sync.Once
	failure       chan error
	failureOnce   sync.Once

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	close  sync.Once
}

// NewLoopbackSession constructs a PeerConnection whose ICE agent is restricted
// to loopback interfaces and loopback IP addresses.
func NewLoopbackSession(role string) (*Session, error) {
	setting := webrtc.SettingEngine{}
	setting.SetInterfaceFilter(isLoopbackInterface)
	setting.SetIPFilter(func(ip net.IP) bool { return ip != nil && ip.IsLoopback() })
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Relay verification intentionally carries duplicate RTP sequence numbers.
	// SRTP replay protection is disabled only on this loopback test transport so
	// the jitter buffer, not the crypto layer, accounts for duplicates.
	setting.DisableSRTPReplayProtection(true)
	mux, err := newLoopbackUDPMux()
	if err != nil {
		return nil, err
	}
	setting.SetICEUDPMux(mux)

	media := &webrtc.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("register codecs: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithSettingEngine(setting))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("create peer connection: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		role:        role,
		pc:          pc,
		mux:         mux,
		remoteTrack: make(chan *webrtc.TrackRemote, 4),
		candidates:  NewCandidateSet(),
		connected:   make(chan struct{}),
		gathered:    make(chan struct{}),
		failure:     make(chan error, 1),
		ctx:         ctx,
		cancel:      cancel,
	}
	s.installHandlers()
	return s, nil
}

func newLoopbackUDPMux() (*ice.MultiUDPMuxDefault, error) {
	netTransport, err := stdnet.NewNet()
	if err != nil {
		return nil, fmt.Errorf("create loopback network: %w", err)
	}
	muxes := make([]ice.UDPMux, 0, 2)
	for i := 0; i < 2; i++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			for _, mux := range muxes {
				_ = mux.Close()
			}
			return nil, fmt.Errorf("bind loopback UDP mux %d: %w", i, err)
		}
		muxes = append(muxes, ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn, Net: netTransport}))
	}
	return ice.NewMultiUDPMuxDefault(muxes...), nil
}

func isLoopbackInterface(name string) bool {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch value := addr.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

func (s *Session) installHandlers() {
	s.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		parsed, err := s.candidates.AddLocal(init.Candidate)
		if err != nil {
			s.fail(fmt.Errorf("local candidate policy: %w", err))
			return
		}
		_ = parsed
		sigCandidate := signal.ICECandidate{
			Candidate:     init.Candidate,
			SDPMid:        init.SDPMid,
			SDPMLineIndex: init.SDPMLineIndex,
		}
		if s.onCandidate != nil {
			s.onCandidate(sigCandidate)
		}
	})
	s.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			s.connectedOnce.Do(func() { close(s.connected) })
		case webrtc.PeerConnectionStateFailed:
			s.fail(fmt.Errorf("%w: peer connection entered failed state", ErrICEFailed))
		case webrtc.PeerConnectionStateClosed:
			s.fail(errors.New("peer: connection closed"))
		}
	})
	s.pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		if state == webrtc.ICEGatheringStateComplete {
			s.gatheredOnce.Do(func() { close(s.gathered) })
		}
	})
	s.pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if receiver != nil {
			s.wg.Add(1)
			go s.drainRTCP(receiver)
		}
		select {
		case s.remoteTrack <- track:
		case <-s.ctx.Done():
		}
	})
}

func (s *Session) fail(err error) {
	s.failureOnce.Do(func() {
		select {
		case s.failure <- err:
		default:
		}
	})
}

// OnLocalCandidate registers the trickle candidate sink. It must be called
// before CreateOffer/AcceptOffer so no candidate is lost.
func (s *Session) OnLocalCandidate(fn func(signal.ICECandidate)) { s.onCandidate = fn }

// AddVideoTrack creates the sender's static RTP track. Call only on the sender.
func (s *Session) AddVideoTrack(id, streamID string) error {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0},
		id, streamID,
	)
	if err != nil {
		return fmt.Errorf("create local track: %w", err)
	}
	sender, err := s.pc.AddTrack(track)
	if err != nil {
		return fmt.Errorf("add local track: %w", err)
	}
	s.senderTrack = track
	s.wg.Add(1)
	go s.drainSenderRTCP(sender)
	return nil
}

// WriteRTP sends one media packet on the sender track.
func (s *Session) WriteRTP(pkt *rtp.Packet) error {
	if s.senderTrack == nil {
		return errors.New("peer: sender track is not configured")
	}
	return s.senderTrack.WriteRTP(pkt)
}

// RemoteTrack returns the first received track.
func (s *Session) RemoteTrack() <-chan *webrtc.TrackRemote { return s.remoteTrack }

// CreateOffer starts negotiation and returns the local SDP offer.
func (s *Session) CreateOffer() (string, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local offer: %w", err)
	}
	return offer.SDP, nil
}

// AcceptOffer applies a remote offer and returns the local SDP answer.
func (s *Session) AcceptOffer(offer string) (string, error) {
	if err := s.setRemote(offer, webrtc.SDPTypeOffer); err != nil {
		return "", err
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer: %w", err)
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set local answer: %w", err)
	}
	return answer.SDP, nil
}

// SetAnswer applies the peer's answer SDP.
func (s *Session) SetAnswer(answer string) error {
	return s.setRemote(answer, webrtc.SDPTypeAnswer)
}

func (s *Session) setRemote(sdp string, kind webrtc.SDPType) error {
	if sdp == "" {
		return fmt.Errorf("%w: empty SDP", signal.ErrInvalidMessage)
	}
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: kind, SDP: sdp}); err != nil {
		return fmt.Errorf("set remote %s: %w", kind, err)
	}
	s.remoteMu.Lock()
	s.remoteSet = true
	queued := append([]signal.ICECandidate(nil), s.queuedCand...)
	s.queuedCand = nil
	s.remoteMu.Unlock()
	for _, c := range queued {
		if err := s.addRemoteNow(c); err != nil {
			return err
		}
	}
	return nil
}

// AddICECandidate accepts a trickle candidate. Candidates arriving before the
// remote description are queued and applied in arrival order afterwards.
func (s *Session) AddICECandidate(c signal.ICECandidate) error {
	if _, err := s.candidates.AddRemote(c.Candidate); err != nil {
		return err
	}
	s.remoteMu.Lock()
	if !s.remoteSet {
		s.queuedCand = append(s.queuedCand, c)
		s.remoteMu.Unlock()
		return nil
	}
	s.remoteMu.Unlock()
	return s.addRemoteNow(c)
}

func (s *Session) addRemoteNow(c signal.ICECandidate) error {
	init := webrtc.ICECandidateInit{
		Candidate:     c.Candidate,
		SDPMid:        c.SDPMid,
		SDPMLineIndex: c.SDPMLineIndex,
	}
	if err := s.pc.AddICECandidate(init); err != nil {
		return fmt.Errorf("add remote candidate: %w", err)
	}
	return nil
}

// CandidateSnapshot exposes gathered candidate counts and banned candidates.
func (s *Session) CandidateSnapshot() (local, remote []ParsedCandidate, banned uint64) {
	return s.candidates.Snapshot()
}

// SelectedPair reports the pair selected by the ICE agent, if any.
func (s *Session) SelectedPair() (SelectedPair, bool) {
	return SelectedPairFromPC(s.pc)
}

// WaitConnected waits for DTLS/ICE media connectivity.
func (s *Session) WaitConnected(ctx context.Context, timeout time.Duration) error {
	if s.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.connected:
		return nil
	case err := <-s.failure:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrConnectionTimeout, timeout)
	}
}

// WaitGatheringComplete waits until the trickle gatherer emits its final host
// candidate. It is used by probe to produce stable checklist counts.
func (s *Session) WaitGatheringComplete(ctx context.Context, timeout time.Duration) error {
	if s.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.gathered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("peer: candidate gathering timeout after %s", timeout)
	}
}

// PeerConnection provides read-only access for diagnostics.
func (s *Session) PeerConnection() *webrtc.PeerConnection { return s.pc }

func (s *Session) drainSenderRTCP(sender *webrtc.RTPSender) {
	defer s.wg.Done()
	for {
		if _, _, err := sender.ReadRTCP(); err != nil {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		default:
		}
	}
}

func (s *Session) drainRTCP(receiver *webrtc.RTPReceiver) {
	defer s.wg.Done()
	for {
		if _, _, err := receiver.ReadRTCP(); err != nil {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		default:
		}
	}
}

// ReadRemoteRTP reads and unmarshals packets from a remote track.
func ReadRemoteRTP(ctx context.Context, track *webrtc.TrackRemote, out chan<- *rtp.Packet) error {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		select {
		case out <- pkt:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close tears down media and all background drainers.
func (s *Session) Close() error {
	var err error
	s.close.Do(func() {
		s.cancel()
		err = s.pc.Close()
		s.wg.Wait()
		if closeErr := s.mux.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
