package relay

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/impair"
	"pairwise.local/relayctl/internal/signal"
)

const MaxEgress = 8

// Config controls per-egress impairment and reordering behavior.
type Config struct {
	Impair        impair.Config
	ReorderWindow uint16
	ReorderHold   time.Duration
	WriteBackoff  time.Duration
}

// DefaultConfig uses a two-packet reorder window (40ms at 50pps).
func DefaultConfig() Config {
	return Config{
		Impair:        impair.Config{MaxExtraDelay: 62 * time.Millisecond},
		ReorderWindow: 2,
		ReorderHold:   40 * time.Millisecond,
		WriteBackoff:  100 * time.Millisecond,
	}
}

// Engine implements signal.Engine and fans one ingress track out to up to eight
// independently rewritten egress tracks.
type Engine struct {
	api            *webrtc.API
	cfg            Config
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	ingress        map[string]*ingress
	egress         map[string]*egress
	order          []*egress
	ingressPackets atomic.Uint64
}

// NewEngine creates a local-only WebRTC API.
func NewEngine(cfg Config) (*Engine, error) {
	api, err := newLoopbackAPI()
	if err != nil {
		return nil, err
	}
	if cfg.ReorderWindow == 0 {
		cfg.ReorderWindow = DefaultConfig().ReorderWindow
	}
	if cfg.ReorderHold <= 0 {
		cfg.ReorderHold = DefaultConfig().ReorderHold
	}
	if cfg.WriteBackoff <= 0 {
		cfg.WriteBackoff = DefaultConfig().WriteBackoff
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{api: api, cfg: cfg, ctx: ctx, cancel: cancel,
		ingress: make(map[string]*ingress), egress: make(map[string]*egress)}, nil
}

func newLoopbackAPI() (*webrtc.API, error) {
	media := &webrtc.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register codecs: %w", err)
	}
	setting := webrtc.SettingEngine{}
	setting.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetIPFilter(func(ip net.IP) bool { return ip.To4() != nil && ip.IsLoopback() })
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithSettingEngine(setting)), nil
}

// NewLoopbackAPI returns the same loopback-only WebRTC configuration used by
// relay peers. CLI publishers and subscribers use it to keep all candidates in
// 127.0.0.0/8.
func NewLoopbackAPI() (*webrtc.API, error) { return newLoopbackAPI() }

// NewPublisher creates the single room ingress.
func (e *Engine) NewPublisher(room, endpoint string) (signal.Peer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ingress[room] != nil {
		return nil, fmt.Errorf("room %s already has an ingress", room)
	}
	peer, err := newIngress(e, room, endpoint)
	if err != nil {
		return nil, err
	}
	e.ingress[room] = peer
	return peer, nil
}

// NewSubscriber creates an egress and its independent packet pipeline.
func (e *Engine) NewSubscriber(room, endpoint string) (signal.Peer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.order) >= MaxEgress {
		return nil, fmt.Errorf("egress capacity reached")
	}
	peer, err := newEgress(e, room, endpoint, len(e.order))
	if err != nil {
		return nil, err
	}
	e.egress[endpoint] = peer
	e.order = append(e.order, peer)
	return peer, nil
}

// RemovePeer closes and forgets an ingress or egress.
func (e *Engine) RemovePeer(room, endpoint string) {
	e.mu.Lock()
	var closing signal.Peer
	if current := e.ingress[room]; current != nil && current.endpoint == endpoint {
		closing = current
		delete(e.ingress, room)
	}
	if current := e.egress[endpoint]; current != nil {
		closing = current
		delete(e.egress, endpoint)
		for i, item := range e.order {
			if item == current {
				e.order = append(e.order[:i], e.order[i+1:]...)
				break
			}
		}
	}
	e.mu.Unlock()
	if closing != nil {
		_ = closing.Close()
	}
}

// Close stops all media and packet workers.
func (e *Engine) Close() error {
	e.cancel()
	e.mu.Lock()
	peers := make([]signal.Peer, 0, len(e.ingress)+len(e.order))
	for _, peer := range e.ingress {
		peers = append(peers, peer)
	}
	for _, peer := range e.order {
		peers = append(peers, peer)
	}
	e.ingress = make(map[string]*ingress)
	e.egress = make(map[string]*egress)
	e.order = nil
	e.mu.Unlock()
	var first error
	for _, peer := range peers {
		if err := peer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (e *Engine) broadcast(packet *rtp.Packet, now time.Time) {
	e.ingressPackets.Add(1)
	e.mu.Lock()
	targets := append([]*egress(nil), e.order...)
	e.mu.Unlock()
	for _, target := range targets {
		target.enqueue(packet, now)
	}
}

// Stats is the aggregate report source.
type Stats struct {
	CandidatePairs   int
	ForwardedPackets uint64
	DroppedPackets   uint64
	LatePackets      uint64
	JitterMS         JitterStats
	EgressFailed     int
	IngressPackets   uint64
	Egresses         []EgressStats
}

// EgressStats contains one outlet's counters.
type EgressStats struct {
	Endpoint  string      `json:"endpoint"`
	SSRC      uint32      `json:"ssrc"`
	Forwarded uint64      `json:"forwarded"`
	Dropped   uint64      `json:"dropped"`
	Late      uint64      `json:"late"`
	JitterMS  JitterStats `json:"jitter_ms"`
	Failed    bool        `json:"failed"`
}

// Snapshot collects counters and selected candidate pairs.
func (e *Engine) Snapshot() Stats {
	e.mu.Lock()
	ingressPeers := make([]*ingress, 0, len(e.ingress))
	for _, peer := range e.ingress {
		ingressPeers = append(ingressPeers, peer)
	}
	egressPeers := append([]*egress(nil), e.order...)
	e.mu.Unlock()
	result := Stats{IngressPackets: e.ingressPackets.Load()}
	for _, peer := range ingressPeers {
		if hasSelectedPair(peer.pc) {
			result.CandidatePairs++
		}
	}
	var jitterValues []float64
	for _, peer := range egressPeers {
		stat := peer.snapshot()
		result.ForwardedPackets += stat.Forwarded
		result.DroppedPackets += stat.Dropped
		result.LatePackets += stat.Late
		if stat.Failed {
			result.EgressFailed++
		}
		jitterValues = append(jitterValues, peer.tracker.Values()...)
		result.Egresses = append(result.Egresses, stat)
		if hasSelectedPair(peer.pc) {
			result.CandidatePairs++
		}
	}
	sort.Slice(result.Egresses, func(i, j int) bool { return result.Egresses[i].Endpoint < result.Egresses[j].Endpoint })
	result.JitterMS = JitterFromValues(jitterValues)
	return result
}

// Drain releases all reorder-buffered packets before final statistics.
func (e *Engine) Drain() {
	e.mu.Lock()
	egressPeers := append([]*egress(nil), e.order...)
	e.mu.Unlock()
	for _, peer := range egressPeers {
		if peer.failed.Load() {
			continue
		}
		ack := make(chan struct{})
		select {
		case peer.flushCh <- ack:
			select {
			case <-ack:
			case <-time.After(time.Second):
			}
		case <-peer.ctx.Done():
		}
	}
}

func hasSelectedPair(pc *webrtc.PeerConnection) bool {
	if pc == nil || pc.SCTP() == nil || pc.SCTP().Transport() == nil {
		return false
	}
	pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	return err == nil && pair != nil
}

type basePeer struct {
	pc        *webrtc.PeerConnection
	mu        sync.Mutex
	onLocal   func(signal.Candidate)
	pending   []signal.Candidate
	closeOnce sync.Once
}

func newBasePeer(pc *webrtc.PeerConnection) *basePeer {
	peer := &basePeer{pc: pc}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		peer.emitCandidate(signal.Candidate{
			Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex,
		})
	})
	return peer
}

func (p *basePeer) emitCandidate(candidate signal.Candidate) {
	p.mu.Lock()
	if p.onLocal == nil {
		p.pending = append(p.pending, candidate)
		p.mu.Unlock()
		return
	}
	fn := p.onLocal
	p.mu.Unlock()
	fn(candidate)
}

func (p *basePeer) OnLocalCandidate(fn func(signal.Candidate)) {
	p.mu.Lock()
	p.onLocal = fn
	pending := append([]signal.Candidate(nil), p.pending...)
	p.pending = nil
	p.mu.Unlock()
	for _, candidate := range pending {
		fn(candidate)
	}
}

func (p *basePeer) AddICECandidate(candidate signal.Candidate) error {
	if err := guard.ValidateCandidate(candidate.Candidate); err != nil {
		return err
	}
	return p.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: candidate.Candidate, SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex,
	})
}

func (p *basePeer) Close() error {
	var err error
	p.closeOnce.Do(func() { err = p.pc.Close() })
	return err
}

func notSupported(operation string) error {
	return fmt.Errorf("%s not supported for this peer role", operation)
}
