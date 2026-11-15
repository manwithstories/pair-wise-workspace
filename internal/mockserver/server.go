// Package mockserver provides a loopback-only deterministic RTSP source.
package mockserver

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"

	"rtspaudit/internal/fixture"
	auditrtsp "rtspaudit/internal/rtsp"
)

const ssrc = 0x51a7c0de

// Config controls generated media and mock behavior.
type Config struct {
	Listen         string
	Frames         int
	Loss           float64
	Reorder        float64
	Duplicate      float64
	Seed           int64
	Policy         string
	SessionTimeout time.Duration
	StallAfter     int
}

// Server serves one SDP track over TCP interleaved or UDP.
type Server struct {
	cfg       Config
	ln        net.Listener
	fixture   *fixture.Fixture
	packets   []fixture.Packet
	sdp       []byte
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	closeOnce sync.Once
	nextID    atomic.Uint64
}

// Start generates the fixture and listens only on 127.0.0.1.
func Start(cfg Config) (*Server, error) {
	if cfg.Frames <= 0 {
		return nil, fmt.Errorf("frames must be positive")
	}
	if cfg.Loss < 0 || cfg.Loss > 1 || cfg.Reorder < 0 || cfg.Reorder > 1 ||
		cfg.Duplicate < 0 || cfg.Duplicate > 1 {
		return nil, fmt.Errorf("impairment probabilities must be in 0..1")
	}
	if cfg.Policy == "" {
		cfg.Policy = "strict"
	}
	if cfg.Policy != "strict" && cfg.Policy != "lenient" {
		return nil, fmt.Errorf("invalid policy %q", cfg.Policy)
	}
	if cfg.SessionTimeout < 0 || cfg.StallAfter < 0 {
		return nil, fmt.Errorf("timeouts and stall-after must be non-negative")
	}
	fx, err := fixture.Generate(cfg.Frames)
	if err != nil {
		return nil, err
	}
	packets := fixture.Apply(fx.Packetize(fixture.FirstSequence(cfg.Seed)), fixture.ImpairOptions{
		Loss: cfg.Loss, Reorder: cfg.Reorder, Duplicate: cfg.Duplicate, Seed: cfg.Seed,
	})
	sdp, err := makeSDP(fx)
	if err != nil {
		return nil, err
	}
	listen := cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if err := auditrtsp.ValidateLoopbackAddr(listen); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", listen)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, ln: ln, fixture: fx, packets: packets, sdp: sdp,
		ctx: ctx, cancel: cancel, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func makeSDP(fx *fixture.Fixture) ([]byte, error) {
	desc := &description.Session{
		Title: "rtspaudit deterministic fixture",
		Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Control: "trackID=0",
			Formats: []format.Format{&format.H264{
				PayloadTyp: 96, SPS: fx.SPS, PPS: fx.PPS, PacketizationMode: 1,
			}},
		}},
	}
	return desc.Marshal(false)
}

// Addr returns the loopback host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// URL returns the RTSP session URL.
func (s *Server) URL() string { return "rtsp://" + s.Addr() + "/audit" }

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.serve(conn)
		}()
	}
}

// Close cancels all sessions and closes every listener/socket.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		err = s.ln.Close()
		s.mu.Lock()
		for conn := range s.conns {
			conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return err
}
