package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtp"

	"relayctl/internal/peer"
	"relayctl/internal/relay"
	"relayctl/internal/signal"
)

func openDemoPeers(ctx context.Context, addr, sessionID string) (*signal.Client, *signal.Client, *peer.Session, *peer.Session, error) {
	relayClient, err := signal.Dial(ctx, addr, sessionID, signal.RoleRelay, signal.ClientOptions{})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	senderClient, err := signal.Dial(ctx, addr, sessionID, signal.RoleSender, signal.ClientOptions{})
	if err != nil {
		_ = relayClient.Close()
		return nil, nil, nil, nil, err
	}
	relayPeer, err := peer.NewLoopbackSession("relay")
	if err != nil {
		_ = senderClient.Close()
		_ = relayClient.Close()
		return nil, nil, nil, nil, err
	}
	senderPeer, err := peer.NewLoopbackSession("sender")
	if err != nil {
		_ = relayPeer.Close()
		_ = senderClient.Close()
		_ = relayClient.Close()
		return nil, nil, nil, nil, err
	}
	return relayClient, senderClient, relayPeer, senderPeer, nil
}

func demoCandidateSink(client *signal.Client, session *peer.Session, fail func(error)) {
	session.OnLocalCandidate(func(candidate signal.ICECandidate) {
		if err := client.Send(&signal.Message{Type: signal.TypeCandidate, Candidate: &candidate}); err != nil {
			fail(err)
		}
	})
}

func demoSenderSignaling(ctx context.Context, client *signal.Client, session *peer.Session, ready chan<- struct{}, fail func(error)) error {
	offered := false
	var readyOnce sync.Once
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-client.Errors():
			return err
		case m, ok := <-client.Messages():
			if !ok {
				return errors.New("signal client closed")
			}
			if err := signalWireError(m); err != nil {
				return err
			}
			switch m.Type {
			case signal.TypeReady:
				if offered {
					continue
				}
				offer, err := session.CreateOffer()
				if err != nil {
					return withCode("sdp_malformed", "inspect generated offer SDP", err)
				}
				if err := client.Send(&signal.Message{Type: signal.TypeOffer, SDP: offer}); err != nil {
					return err
				}
				offered = true
			case signal.TypeAnswer:
				if err := session.SetAnswer(m.SDP); err != nil {
					return withCode("sdp_malformed", "inspect answer SDP", err)
				}
				go func() {
					err := session.WaitConnected(ctx, 12*time.Second)
					if err != nil {
						if ctx.Err() == nil {
							fail(err)
						}
						return
					}
					readyOnce.Do(func() { close(ready) })
				}()
			case signal.TypeCandidate:
				if m.Candidate != nil {
					if err := session.AddICECandidate(*m.Candidate); err != nil {
						return fmt.Errorf("add sender candidate: %w", err)
					}
				}
			}
		}
	}
}

func demoRelaySignaling(ctx context.Context, client *signal.Client, session *peer.Session, ready chan<- struct{}, fail func(error)) error {
	var readyOnce sync.Once
	offered := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-client.Errors():
			return err
		case m, ok := <-client.Messages():
			if !ok {
				return errors.New("signal client closed")
			}
			if err := signalWireError(m); err != nil {
				return err
			}
			switch m.Type {
			case signal.TypeOffer:
				if offered {
					continue
				}
				answer, err := session.AcceptOffer(m.SDP)
				if err != nil {
					return withCode("sdp_malformed", "inspect offer SDP", err)
				}
				if err := client.Send(&signal.Message{Type: signal.TypeAnswer, SDP: answer}); err != nil {
					return err
				}
				offered = true
				go func() {
					err := session.WaitConnected(ctx, 12*time.Second)
					if err != nil {
						if ctx.Err() == nil {
							fail(err)
						}
						return
					}
					readyOnce.Do(func() { close(ready) })
				}()
			case signal.TypeCandidate:
				if m.Candidate != nil {
					if err := session.AddICECandidate(*m.Candidate); err != nil {
						return fmt.Errorf("add relay candidate: %w", err)
					}
				}
			}
		}
	}
}

func waitDemoReady(ctx context.Context, errCh <-chan error, senderReady, relayReady <-chan struct{}) error {
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for senderReady != nil || relayReady != nil {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%w: negotiation did not complete", signal.ErrSignalTimeout)
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		case <-senderReady:
			senderReady = nil
		case <-relayReady:
			relayReady = nil
		}
	}
	return nil
}

func runRelayPipeline(ctx context.Context, packets <-chan *rtp.Packet, stop <-chan struct{}, stats *relay.Stats, forwarder *relay.Forwarder) error {
	jitter := relay.NewJitterBuffer(stats)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	process := func(pkts []*rtp.Packet) error {
		for _, pkt := range pkts {
			if err := forwarder.Forward(pkt); err != nil {
				return withCode("sink_unavailable", "start relayctl sink or use --sink 127.0.0.1:0", err)
			}
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return demoContextError(ctx)
		case pkt, ok := <-packets:
			if !ok {
				if err := process(jitter.FlushAll()); err != nil {
					return err
				}
				return nil
			}
			if err := process(jitter.Push(pkt, time.Now())); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := process(jitter.Flush(now)); err != nil {
				return err
			}
		case <-stop:
			idle := time.NewTimer(150 * time.Millisecond)
			defer idle.Stop()
			for {
				select {
				case pkt, ok := <-packets:
					if !ok {
						return process(jitter.FlushAll())
					}
					if !idle.Stop() {
						<-idle.C
					}
					idle.Reset(150 * time.Millisecond)
					if err := process(jitter.Push(pkt, time.Now())); err != nil {
						return err
					}
				case <-idle.C:
					return process(jitter.FlushAll())
				case <-ctx.Done():
					return demoContextError(ctx)
				}
			}
		}
	}
}
