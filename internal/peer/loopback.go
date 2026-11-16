// Package peer wires two Pion PeerConnections over loopback so the rest of the
// program can exchange RTP without any external signaling service.
package peer

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// Defaults for the loopback pair. They are short because both endpoints sit on
// the same host and the ICE checklist has exactly one viable path.
const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultMediaTimeout   = 5 * time.Second
	VideoClockRate        = 90000

	iceDisconnectedTimeout = 5 * time.Second
	iceFailedTimeout       = 10 * time.Second
	iceKeepaliveInterval   = 1 * time.Second
)

// Sentinel errors so the CLI can map a failure onto its exit code without
// parsing strings.
var (
	// ErrICETimeout means ICE never produced a connected candidate pair.
	ErrICETimeout = errors.New("ICE negotiation timed out")
	// ErrDTLSFailed means ICE connected but the DTLS handshake did not.
	ErrDTLSFailed = errors.New("DTLS handshake failed")
	// ErrNonLoopbackCandidate means a candidate outside 127.0.0.1/::1 was
	// offered. It is a hard failure, never a warning.
	ErrNonLoopbackCandidate = errors.New("non-loopback ICE candidate rejected")
	// ErrClosed means the pair was already closed.
	ErrClosed = errors.New("loopback pair closed")
)

// Candidate is a local ICE candidate as the pair observed it. Raw is the SDP
// attribute form (without the "candidate:" prefix fastidiously re-added by
// Pion), which is what the signaling protocol carries verbatim.
type Candidate struct {
	Side    string
	Address string
	Port    uint16
	Type    webrtc.ICECandidateType
	Raw     string
}

// Config tunes the loopback pair.
type Config struct {
	// ConnectTimeout bounds the whole ICE + DTLS exchange.
	ConnectTimeout time.Duration
	// MediaTimeout bounds how long Track waits for the remote track.
	MediaTimeout time.Duration
	// OnCandidate is called for every local candidate gathered, both for
	// observability and so the caller can record the addresses used.
	OnCandidate func(Candidate)
	// RejectCandidate is consulted before a remote candidate is applied. A
	// non-nil error refuses the candidate and records it as rejected.
	RejectCandidate func(Candidate) error
	// SSRC, when non-zero, is pinned as the outbound SSRC before the offer is
	// created, so the SDP declares it and the receiver sees the fixture's
	// SSRC on the wire instead of Pion's random assignment.
	SSRC uint32
	// RelayOnly suppresses the direct peer-to-peer candidate handoff: gathered
	// candidates are recorded and passed to OnCandidate, but the caller is
	// responsible for delivering them (typically through the signaling
	// server). It is what makes the signaling path real rather than decorative.
	RelayOnly bool
}

// Pair is two connected PeerConnections. Sender owns the outbound track;
// Receiver yields the inbound track.
type Pair struct {
	Sender   *webrtc.PeerConnection
	Receiver *webrtc.PeerConnection
	Track    *webrtc.TrackLocalStaticRTP

	ssrc        uint32
	ssrcApplied atomic.Bool

	mu         sync.Mutex
	candidates []Candidate
	rejected   []string
	localAddr  string
	remoteAddr string
	closed     bool
}

// Connect builds both peers, negotiates directly in memory and waits until
// media can flow. It is the one-shot entry point used by tests and the demo.
func Connect(cfg Config) (*Pair, error) {
	p, err := NewPair(cfg)
	if err != nil {
		return nil, err
	}
	if err := p.Negotiate(); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// NewPair creates both PeerConnections and adds the outbound track, but does
// no negotiation. Use it when SDP and candidates travel through the signaling
// server instead of directly.
func NewPair(cfg Config) (*Pair, error) {
	cfg = withDefaults(cfg)
	sender, err := newEndpoint()
	if err != nil {
		return nil, fmt.Errorf("create sender: %w", err)
	}
	receiver, err := newEndpoint()
	if err != nil {
		_ = sender.Close()
		return nil, fmt.Errorf("create receiver: %w", err)
	}
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: VideoClockRate}
	track, err := webrtc.NewTrackLocalStaticRTP(codec, "loopback-video", "loopback-stream")
	if err != nil {
		_ = sender.Close()
		_ = receiver.Close()
		return nil, fmt.Errorf("create track: %w", err)
	}
	if _, err := sender.AddTrack(track); err != nil {
		_ = sender.Close()
		_ = receiver.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}

	p := &Pair{Sender: sender, Receiver: receiver, Track: track, ssrc: cfg.SSRC}
	p.installHandlers(cfg)
	return p, nil
}

func withDefaults(cfg Config) Config {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if cfg.MediaTimeout <= 0 {
		cfg.MediaTimeout = DefaultMediaTimeout
	}
	return cfg
}

// newEndpoint builds a PeerConnection restricted to loopback: the IP filter
// rejects every other address at gather time, and mDNS is disabled so
// candidate addresses are always literal IPs that can be checked.
func newEndpoint() (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval)
	// The fixture deliberately replays sequence numbers; SRTP replay
	// protection would silently discard those packets before the analyser
	// ever saw them, hiding the duplicate statistic we are asked to measure.
	se.DisableSRTPReplayProtection(true)
	// Fire OnTrack before the first RTP packet arrives, so the start of a
	// burst is not lost to handler setup.
	se.SetFireOnTrackBeforeFirstRTP(true)

	ir := &interceptor.Registry{}
	return webrtc.NewAPI(
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	).NewPeerConnection(webrtc.Configuration{})
}

// installHandlers exchanges candidates directly between the two peers and
// records each one. A refused candidate is never handed to the peer.
func (p *Pair) installHandlers(cfg Config) {
	record := func(side string) func(*webrtc.ICECandidate) {
		return func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			init := c.ToJSON()
			cand := Candidate{Side: side, Address: c.Address, Port: c.Port, Type: c.Typ, Raw: init.Candidate}
			if cfg.RejectCandidate != nil {
				if err := cfg.RejectCandidate(cand); err != nil {
					p.mu.Lock()
					p.rejected = append(p.rejected, fmt.Sprintf("%s %s: %v", side, c.Address, err))
					p.mu.Unlock()
					return
				}
			}
			p.mu.Lock()
			p.candidates = append(p.candidates, cand)
			p.mu.Unlock()
			if cfg.OnCandidate != nil {
				cfg.OnCandidate(cand)
			}
			if cfg.RelayOnly {
				return
			}
			other := p.Receiver
			if side == "receiver" {
				other = p.Sender
			}
			_ = other.AddICECandidate(init)
		}
	}
	p.Sender.OnICECandidate(record("sender"))
	p.Receiver.OnICECandidate(record("receiver"))
}
