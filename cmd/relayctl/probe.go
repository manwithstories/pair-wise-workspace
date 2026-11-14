package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"relayctl/internal/peer"
	"relayctl/internal/signal"
)

type probeReport struct {
	Schema           string `json:"schema"`
	Bind             string `json:"bind"`
	WantPairs        int    `json:"want_pairs"`
	LocalCandidates  int    `json:"local_candidates"`
	RemoteCandidates int    `json:"remote_candidates"`
	PairsChecked     int    `json:"pairs_checked"`
	PairsSucceeded   int    `json:"pairs_succeeded"`
	Selected         string `json:"selected"`
	BannedCandidates uint64 `json:"banned_candidates"`
}

func runProbe(args []string, stdout io.Writer) error {
	fs := newFlagSet("probe", stdout)
	bind := fs.String("bind", "127.0.0.1:0", "loopback signalling bind address")
	wantPairs := fs.Int("want-pairs", 2, "target candidate pair count for the report")
	jsonPath := fs.String("json", "", "JSON report path")
	injectBanned := fs.Bool("inject-banned", false, "send one non-loopback candidate to exercise policy rejection")
	if err := fs.Parse(args); err != nil {
		return withCode("bad_args", "see relayctl probe -h", err)
	}
	if fs.NArg() != 0 {
		return withCode("bad_args", "unexpected probe arguments", errors.New(fs.Args()[0]))
	}
	if err := validateLoopbackAddr(*bind); err != nil {
		return err
	}
	if *wantPairs < 1 || *wantPairs > 4 {
		return withCode("bad_args", "want-pairs must be between 1 and 4", fmt.Errorf("got %d", *wantPairs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server, err := signal.Listen(*bind, signal.Options{})
	if err != nil {
		return fmt.Errorf("start probe signalling: %w", err)
	}
	defer server.Close()

	relayClient, senderClient, relayPeer, senderPeer, err := openProbePeers(ctx, server.Addr().String())
	if err != nil {
		return err
	}
	defer senderClient.Close()
	defer relayClient.Close()
	defer senderPeer.Close()
	defer relayPeer.Close()
	if err := senderPeer.AddVideoTrack("probe-video", "probe"); err != nil {
		return err
	}

	errCh := make(chan error, 4)
	senderReady := make(chan struct{})
	relayReady := make(chan struct{})
	candidateSink := func(client *signal.Client) func(signal.ICECandidate) {
		return func(candidate signal.ICECandidate) {
			if err := client.Send(&signal.Message{Type: signal.TypeCandidate, Candidate: &candidate}); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}
	}
	senderPeer.OnLocalCandidate(candidateSink(senderClient))
	relayPeer.OnLocalCandidate(candidateSink(relayClient))
	go func() { errCh <- probeSenderLoop(ctx, senderClient, senderPeer, senderReady, errCh, *injectBanned) }()
	go func() { errCh <- probeRelayLoop(ctx, relayClient, relayPeer, relayReady, errCh) }()

	if err := waitProbeReady(ctx, senderReady, relayReady, errCh); err != nil {
		return err
	}
	if err := senderPeer.WaitGatheringComplete(ctx, 3*time.Second); err != nil {
		return err
	}
	if err := relayPeer.WaitGatheringComplete(ctx, 3*time.Second); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)

	if *injectBanned {
		raw := "candidate:1 1 UDP 2122260223 192.0.2.10 5000 typ host"
		_ = senderClient.Send(&signal.Message{Type: signal.TypeCandidate, Candidate: &signal.ICECandidate{Candidate: raw}})
		time.Sleep(100 * time.Millisecond)
	}

	local, remote, senderBanned := senderPeer.CandidateSnapshot()
	_, _, relayBanned := relayPeer.CandidateSnapshot()
	selected, ok := senderPeer.SelectedPair()
	if !ok {
		if selected, ok = relayPeer.SelectedPair(); !ok {
			return withCode("ice_failed", "no selected loopback candidate pair", peer.ErrICEFailed)
		}
	}
	pairsChecked := len(peer.EnumerateCandidatePairs(local, remote))
	if pairsChecked == 0 {
		pairsChecked = 1
	}
	pairsSucceeded := 0
	if ok {
		pairsSucceeded = 1
	}
	banned := server.BannedCandidates() + senderBanned + relayBanned
	report := probeReport{
		Schema:           "relayctl.probe.v1",
		Bind:             server.Addr().String(),
		WantPairs:        *wantPairs,
		LocalCandidates:  len(local),
		RemoteCandidates: len(remote),
		PairsChecked:     pairsChecked,
		PairsSucceeded:   pairsSucceeded,
		Selected:         selected.Local,
		BannedCandidates: banned,
	}
	if err := writeJSON(*jsonPath, report); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "local_candidates=%d remote_candidates=%d pairs_checked=%d pairs_succeeded=%d selected=%s banned_candidates=%d\n",
		report.LocalCandidates, report.RemoteCandidates, report.PairsChecked, report.PairsSucceeded, report.Selected, report.BannedCandidates)
	if banned > 0 {
		return fmt.Errorf("%w: banned_candidates=%d", signal.ErrNonLoopbackCandidate, banned)
	}
	if !ok {
		return withCode("ice_failed", "selected pair missing", peer.ErrICEFailed)
	}
	return nil
}

func openProbePeers(ctx context.Context, addr string) (*signal.Client, *signal.Client, *peer.Session, *peer.Session, error) {
	relayClient, err := signal.Dial(ctx, addr, "probe", signal.RoleRelay, signal.ClientOptions{})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	senderClient, err := signal.Dial(ctx, addr, "probe", signal.RoleSender, signal.ClientOptions{})
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

func probeSenderLoop(ctx context.Context, client *signal.Client, session *peer.Session, ready chan<- struct{}, errCh chan<- error, ignoreBanned bool) error {
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
			if m.Type == signal.TypeError && m.Code == signal.CodeCandidateBanned && ignoreBanned {
				continue
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
					return withCode("sdp_malformed", "inspect generated offer", err)
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
					if err := session.WaitConnected(ctx, 10*time.Second); err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
					readyOnce.Do(func() { close(ready) })
				}()
			case signal.TypeCandidate:
				if m.Candidate != nil {
					if err := session.AddICECandidate(*m.Candidate); err != nil {
						return err
					}
				}
			}
		}
	}
}

func probeRelayLoop(ctx context.Context, client *signal.Client, session *peer.Session, ready chan<- struct{}, errCh chan<- error) error {
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
					if err := session.WaitConnected(ctx, 10*time.Second); err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
					readyOnce.Do(func() { close(ready) })
				}()
			case signal.TypeCandidate:
				if m.Candidate != nil {
					if err := session.AddICECandidate(*m.Candidate); err != nil {
						return err
					}
				}
			}
		}
	}
}

func waitProbeReady(ctx context.Context, senderReady, relayReady <-chan struct{}, errCh <-chan error) error {
	for senderReady != nil || relayReady != nil {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w while waiting for probe negotiation", signal.ErrSignalTimeout)
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
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
