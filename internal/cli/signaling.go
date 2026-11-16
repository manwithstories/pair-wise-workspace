package cli

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/peer"
	"github.com/manwithstories/pair-wise-workspace/internal/signaling"
	"github.com/pion/webrtc/v4"
)

// exchangeSDP drives a real offer/answer/candidate exchange through the
// signaling server and returns the connected pair.
//
// Both peers are RelayOnly: they record their candidates but never hand them
// to the other endpoint directly, so the ICE path only completes if the
// signaling server genuinely relays SDP and candidates. The demo relies on
// that, which keeps the server on the critical path instead of decorative.
func exchangeSDP(addr string, opts *Options) (*peer.Pair, error) {
	pair, err := peer.NewPair(peer.Config{
		ConnectTimeout: opts.ConnectTimeout(),
		MediaTimeout:   opts.MediaTimeout(),
		RelayOnly:      true,
		SSRC:           opts.SSRC,
	})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*peer.Pair, error) {
		_ = pair.Close()
		return nil, err
	}

	offerer, err := signaling.Dial(addr)
	if err != nil {
		return fail(fmt.Errorf("signaling dial: %w", err))
	}
	defer offerer.Close()
	answerer, err := signaling.Dial(addr)
	if err != nil {
		return fail(fmt.Errorf("signaling dial: %w", err))
	}
	defer answerer.Close()

	deadline := time.Now().Add(opts.ConnectTimeout())
	session, err := offerSignaling(offerer, pair, deadline)
	if err != nil {
		return fail(err)
	}
	if err := answerSignaling(answerer, pair, session, deadline); err != nil {
		return fail(err)
	}
	// The answer is the offerer's next message; apply it before anything else
	// touches that socket, since the drain loop below consumes blindly.
	if err := awaitAnswer(offerer, pair, deadline); err != nil {
		return fail(err)
	}

	// Both ends now run their candidate drain concurrently with the
	// connection check. The first terminal event cancels the rest.
	stop := make(chan struct{})
	var once sync.Once
	var drainErr error
	halt := func(err error) {
		if err == nil {
			return
		}
		once.Do(func() { drainErr = err; close(stop) })
	}
	var wg sync.WaitGroup
	wg.Add(2)
	// Each socket carries candidates produced by the far side: the offerer's
	// socket receives receiver candidates and vice versa.
	go func() { defer wg.Done(); halt(drainCandidates(offerer, pair, "receiver", stop)) }()
	go func() { defer wg.Done(); halt(drainCandidates(answerer, pair, "sender", stop)) }()

	err = pair.WaitConnected(opts.ConnectTimeout())
	once.Do(func() { close(stop) })
	wg.Wait()
	if err != nil {
		return fail(err)
	}
	if drainErr != nil {
		return fail(drainErr)
	}
	return pair, nil
}

// offerSignaling runs the offering half and returns the session id.
func offerSignaling(c *signaling.Client, pair *peer.Pair, deadline time.Time) (string, error) {
	offer, err := pair.CreateOffer()
	if err != nil {
		return "", err
	}
	if err := c.Send(signaling.Message{Type: signaling.KindOffer, SDP: offer.SDP}); err != nil {
		return "", fmt.Errorf("send offer: %w", err)
	}
	ack, err := c.Recv(deadline)
	if err != nil {
		return "", fmt.Errorf("await offer ack: %w", err)
	}
	if ack.Type == signaling.KindError {
		return "", fmt.Errorf("offer rejected: %s (%s)", ack.Detail, ack.Code)
	}
	if ack.Session == "" {
		return "", fmt.Errorf("signaling: empty session id")
	}
	if err := sendCandidates(c, pair, "sender", ack.Session); err != nil {
		return "", err
	}
	return ack.Session, nil
}

// answerSignaling runs the answering half: join, apply the relayed offer,
// trickle candidates and post the answer.
func answerSignaling(c *signaling.Client, pair *peer.Pair, session string, deadline time.Time) error {
	if err := c.Send(signaling.Message{Type: signaling.KindState, Session: session, State: "join"}); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	relayed, err := c.Recv(deadline)
	if err != nil {
		return fmt.Errorf("await offer relay: %w", err)
	}
	if relayed.Type != signaling.KindOffer {
		return fmt.Errorf("signaling: expected offer relay, got %s", relayed.Type)
	}
	if err := pair.ApplyRemoteOffer(webrtcSession(webrtc.SDPTypeOffer, relayed.SDP)); err != nil {
		return err
	}
	if _, err := c.Recv(deadline); err != nil { // join acknowledgement
		return fmt.Errorf("await join ack: %w", err)
	}
	// Candidates only exist once a local description is set, so the answer is
	// created first and candidates are trickled after it.
	answer, err := pair.CreateAnswer()
	if err != nil {
		return err
	}
	// The answer goes first so the offerer's single-threaded read sees it
	// before any trickled candidate; candidates are still sent after it.
	if err := c.Send(signaling.Message{Type: signaling.KindAnswer, Session: session, SDP: answer.SDP}); err != nil {
		return fmt.Errorf("send answer: %w", err)
	}
	return sendCandidates(c, pair, "receiver", session)
}

// awaitAnswer reads the relayed answer and installs it on the sender.
func awaitAnswer(c *signaling.Client, pair *peer.Pair, deadline time.Time) error {
	msg, err := c.Recv(deadline)
	if err != nil {
		return fmt.Errorf("await answer: %w", err)
	}
	if msg.Type == signaling.KindError {
		return fmt.Errorf("answer rejected: %s (%s)", msg.Detail, msg.Code)
	}
	if msg.Type != signaling.KindAnswer {
		return fmt.Errorf("signaling: expected answer, got %s", msg.Type)
	}
	return pair.ApplyAnswer(webrtcSession(webrtc.SDPTypeAnswer, msg.SDP))
}

// sendCandidates waits for gather, then trickles every candidate for side.
func sendCandidates(c *signaling.Client, pair *peer.Pair, side, session string) error {
	cands := gather(pair, side, 3*time.Second)
	if len(cands) == 0 {
		return fmt.Errorf("no %s candidates gathered", side)
	}
	for _, raw := range cands {
		if err := c.Send(signaling.Message{
			Type: signaling.KindCandidate, Session: session, Candidate: raw,
		}); err != nil {
			return fmt.Errorf("send %s candidate: %w", side, err)
		}
	}
	return nil
}

// gather polls the pair until at least one candidate exists for side.
func gather(pair *peer.Pair, side string, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		if cands := pair.LocalCandidates(side); len(cands) > 0 {
			return cands
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// drainCandidates applies relayed candidates until stop is signalled or the
// socket closes. A read timeout means "no candidate yet", never a failure:
// trickle is allowed to finish quietly.
// originSide names the side that produced the candidates arriving on c.
func drainCandidates(c *signaling.Client, pair *peer.Pair, originSide string, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		msg, err := c.Recv(time.Now().Add(250 * time.Millisecond))
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil // socket closed during shutdown
		}
		switch msg.Type {
		case signaling.KindCandidate:
			if err := pair.AddRemoteCandidate(originSide, msg.Candidate); err != nil {
				return err
			}
		case signaling.KindError:
			return fmt.Errorf("signaling error: %s (%s)", msg.Detail, msg.Code)
		}
	}
}

// webrtcSession wraps an SDP body in the type Pion expects for SetRemote.
func webrtcSession(t webrtc.SDPType, sdp string) webrtc.SessionDescription {
	return webrtc.SessionDescription{Type: t, SDP: sdp}
}
