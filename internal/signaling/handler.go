package signaling

import "time"

// handle dispatches one validated message and enforces per-sender ordering.
func (s *Server) handle(sc *serverConn, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if msg.Type != KindOffer {
		sess := s.sessions[msg.Session]
		if sess == nil {
			return newProtocolError(ErrCodeUnknownSession, "session %q is not registered", msg.Session)
		}
		if err := checkOrder(sess, sc, msg); err != nil {
			return err
		}
		// Only a message that got past the ordering check advances the
		// high-water mark, so a rejected replay cannot disturb the stream.
		sess.lastSeq[sc] = msg.Seq
		sess.lastSeen = time.Now()
	}

	switch msg.Type {
	case KindOffer:
		return s.handleOffer(sc, msg)
	case KindState:
		return s.handleState(sc, msg)
	case KindAnswer:
		return s.handleAnswer(sc, msg)
	case KindCandidate:
		return s.handleCandidate(sc, msg)
	default:
		return newProtocolError(ErrCodeUnknownType, "cannot relay %q", msg.Type)
	}
}

// checkOrder enforces strictly increasing sequence numbers per sender so a
// duplicate delivery is visible instead of being applied twice.
func checkOrder(sess *session, sc *serverConn, msg Message) error {
	last, seen := sess.lastSeq[sc]
	if seen {
		switch {
		case msg.Seq == last:
			return newProtocolError(ErrCodeDuplicate, "seq %d already processed", msg.Seq)
		case msg.Seq < last:
			return newProtocolError(ErrCodeOutOfOrder, "seq %d arrived after %d", msg.Seq, last)
		}
	}
	return nil
}

func (s *Server) handleOffer(sc *serverConn, msg Message) error {
	if msg.Session != "" {
		return newProtocolError(ErrCodeSessionMismatch, "offer must open a new session")
	}
	id, err := newSessionID()
	if err != nil {
		return newProtocolError(ErrCodeInternal, "session id: %s", err)
	}
	// The offer registers the connection but not a sequence number: the next
	// message from this same client may legitimately be seq+1, and pre-seeding
	// the map with anything else would misclassify it.
	sess := &session{
		id:       id,
		offer:    msg,
		offerTo:  sc,
		lastSeq:  make(map[*serverConn]uint64),
		created:  s.now(),
		lastSeen: s.now(),
		pending:  make(map[*serverConn][]Message),
	}
	s.sessions[id] = sess
	s.sessionsCreated.Add(1)
	sc.session = id
	return sc.write(Message{Type: KindState, Session: id, State: stateOfferAccepted})
}

func (s *Server) handleState(sc *serverConn, msg Message) error {
	if msg.State != stateJoin {
		return newProtocolError(ErrCodeUnknownType, "unsupported state %q", msg.State)
	}
	sess := s.sessions[msg.Session]
	if sess.answerTo != nil && sess.answerTo != sc {
		return newProtocolError(ErrCodeDuplicate, "session already has an answerer")
	}
	sess.answerTo = sc
	sc.session = msg.Session
	// Hand the stored offer to the joining peer, then flush candidates that
	// were trickled before it arrived. Both go through deliver so the
	// forwarded counter describes every relayed message, not some of them.
	if err := s.deliver(sess, sc, Message{Type: KindOffer, Session: sess.id, SDP: sess.offer.SDP}); err != nil {
		return err
	}
	if err := s.deliver(sess, sc, Message{Type: KindState, Session: sess.id, State: stateJoin}); err != nil {
		return err
	}
	s.flushPending(sess, sc)
	return nil
}

func (s *Server) handleAnswer(sc *serverConn, msg Message) error {
	sess := s.sessions[msg.Session]
	if sess.answerTo != nil && sess.answerTo != sc {
		return newProtocolError(ErrCodeSessionMismatch, "answer from a non-joined connection")
	}
	sess.answerTo = sc
	sc.session = msg.Session
	if err := s.deliver(sess, sess.offerTo, Message{
		Type: KindAnswer, Session: sess.id, SDP: msg.SDP,
	}); err != nil {
		return err
	}
	return s.deliver(sess, sc, Message{Type: KindState, Session: sess.id, State: stateAnswerAccepted})
}

func (s *Server) handleCandidate(sc *serverConn, msg Message) error {
	if ok, detail := IsLoopbackCandidate(msg.Candidate); !ok {
		return newProtocolError(ErrCodeNonLoopback, "candidate rejected: %s", detail)
	}
	sess := s.sessions[msg.Session]
	peer := s.peerOf(sess, sc)
	out := Message{Type: KindCandidate, Session: sess.id, Candidate: msg.Candidate}
	if peer == nil {
		sess.pending[sc] = append(sess.pending[sc], out)
		return nil
	}
	return s.deliver(sess, peer, out)
}

func (s *Server) peerOf(sess *session, sc *serverConn) *serverConn {
	switch sc {
	case sess.offerTo:
		return sess.answerTo
	case sess.answerTo:
		return sess.offerTo
	default:
		return nil
	}
}

func (s *Server) flushPending(sess *session, to *serverConn) {
	for from, msgs := range sess.pending {
		if from == to {
			continue
		}
		for _, msg := range msgs {
			if err := to.write(msg); err != nil {
				return
			}
		}
		delete(sess.pending, from)
	}
}

func (s *Server) deliver(sess *session, to *serverConn, msg Message) error {
	if to == nil {
		return nil
	}
	if err := to.write(msg); err != nil {
		return newProtocolError(ErrCodeClosed, "peer write failed: %s", err)
	}
	s.messagesForwarded.Add(1)
	sess.lastSeen = s.now()
	return nil
}

// dropConn detaches a closed connection and tells the peer, so neither side
// waits on a dead socket until the idle timeout fires.
func (s *Server) dropConn(sc *serverConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.offerTo != sc && sess.answerTo != sc {
			continue
		}
		var peer *serverConn
		if sess.offerTo == sc {
			peer = sess.answerTo
		} else {
			peer = sess.offerTo
		}
		if peer != nil {
			_ = peer.write(Message{Type: KindState, Session: id, State: statePeerClosed})
		}
		delete(s.sessions, id)
	}
}
