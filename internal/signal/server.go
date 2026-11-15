package signal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const DefaultHeartbeatTimeout = 30 * time.Second

type ServerConfig struct {
	HeartbeatTimeout time.Duration
	HandshakeTimeout time.Duration
}

type Server struct {
	listener         net.Listener
	httpServer       *http.Server
	heartbeatTimeout time.Duration
	handshakeTimeout time.Duration
	upgrader         websocket.Upgrader

	mu      sync.Mutex
	streams map[string]*stream
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup
}

type stream struct {
	mu    sync.Mutex
	peers map[string]*session
}

type session struct {
	conn     *websocket.Conn
	stream   string
	role     string
	source   string
	lastSeen time.Time
	writeMu  sync.Mutex
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &Server{
		listener:         listener,
		heartbeatTimeout: cfg.HeartbeatTimeout,
		handshakeTimeout: cfg.HandshakeTimeout,
		upgrader:         websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		streams:          make(map[string]*stream),
		done:             make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", server.handleWebSocket)
	server.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	server.wg.Add(2)
	go server.serve()
	go server.cleanupLoop()
	return server, nil
}

func (s *Server) URL() string {
	return "ws://" + s.listener.Addr().String() + "/ws"
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	for _, st := range s.streams {
		st.mu.Lock()
		for _, peer := range st.peers {
			_ = peer.conn.Close()
		}
		st.peers = make(map[string]*session)
		st.mu.Unlock()
	}
	s.streams = make(map[string]*stream)
	s.mu.Unlock()

	_ = s.httpServer.Close()
	_ = s.listener.Close()
	s.wg.Wait()
	return nil
}

func (s *Server) serve() {
	defer s.wg.Done()
	err := s.httpServer.Serve(s.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.done)
		}
		s.mu.Unlock()
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	streamName := r.URL.Query().Get("stream")
	role := r.URL.Query().Get("role")
	if streamName == "" || role == "" {
		http.Error(w, "stream and role are required", http.StatusBadRequest)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	peer := &session{conn: conn, stream: streamName, role: role, lastSeen: time.Now()}
	_ = conn.SetReadDeadline(time.Now().Add(s.handshakeTimeout))
	env, err := s.readEnvelope(conn)
	if err != nil || env.Type != TypeHello || env.Stream != streamName {
		_ = conn.Close()
		return
	}
	var hello Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil || hello.Version != ProtocolVersion || hello.Role != role {
		_ = conn.Close()
		return
	}
	peer.source = env.Source
	if peer.source == "" {
		peer.source = role
	}
	if err := s.register(peer); err != nil {
		_ = conn.Close()
		return
	}
	defer s.unregister(peer)
	welcome, _ := json.Marshal(Welcome{Version: ProtocolVersion, Stream: streamName, Role: role})
	if err := peer.writeEnvelope(Envelope{
		Version: ProtocolVersion, Type: TypeWelcome, Stream: streamName,
		Payload: welcome,
	}); err != nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.heartbeatTimeout))
		env, err := s.readEnvelope(conn)
		if err != nil {
			return
		}
		s.touch(peer)
		if env.Type == TypeBye {
			return
		}
		if env.Type == TypeHeartbeat {
			continue
		}
		if env.Source == "" {
			env.Source = peer.source
		}
		if s.relay(peer, env) != nil {
			return
		}
	}
}

func (s *Server) readEnvelope(conn *websocket.Conn) (Envelope, error) {
	_, data, err := conn.ReadMessage()
	if err != nil {
		return Envelope{}, err
	}
	return UnmarshalEnvelope(data)
}

func (s *Server) register(peer *session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("signal server closed")
	}
	st, ok := s.streams[peer.stream]
	if !ok {
		st = &stream{peers: make(map[string]*session)}
		s.streams[peer.stream] = st
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if old := st.peers[peer.role]; old != nil {
		_ = old.conn.Close()
	}
	st.peers[peer.role] = peer
	return nil
}

func (s *Server) unregister(peer *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streams[peer.stream]
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.peers[peer.role] == peer {
		delete(st.peers, peer.role)
	}
	if len(st.peers) == 0 {
		delete(s.streams, peer.stream)
	}
}

func (s *Server) touch(peer *session) {
	s.mu.Lock()
	peer.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *Server) relay(sender *session, env Envelope) error {
	s.mu.Lock()
	st := s.streams[sender.stream]
	s.mu.Unlock()
	if st == nil {
		return fmt.Errorf("stream %q not registered", sender.stream)
	}
	st.mu.Lock()
	recipients := make([]*session, 0, len(st.peers))
	for role, peer := range st.peers {
		if role != sender.role {
			recipients = append(recipients, peer)
		}
	}
	st.mu.Unlock()
	for _, peer := range recipients {
		env.Stream = sender.stream
		if err := peer.writeEnvelope(env); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) cleanupLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.cleanupExpired()
		}
	}
}

func (s *Server) cleanupExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, st := range s.streams {
		st.mu.Lock()
		for role, peer := range st.peers {
			if now.Sub(peer.lastSeen) > s.heartbeatTimeout {
				_ = peer.conn.Close()
				delete(st.peers, role)
			}
		}
		if len(st.peers) == 0 {
			delete(s.streams, name)
		}
		st.mu.Unlock()
	}
}

func (s *session) writeEnvelope(env Envelope) error {
	data, err := MarshalEnvelope(env)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteMessage(websocket.TextMessage, data)
}
