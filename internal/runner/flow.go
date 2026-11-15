package runner

import (
	"context"
	"fmt"
	"sync"

	loopice "rtprelay/internal/ice"
	"rtprelay/internal/peer"
	"rtprelay/internal/report"
	"rtprelay/internal/signal"

	"github.com/pion/webrtc/v4"
)

type negotiation struct {
	offerSDP  string
	answerSDP string
	trickleTX uint64
}

func runSelfTest(ctx context.Context, raw Options) Result {
	opts := normalize(raw)
	base := baseReport(opts)
	if opts.PPS <= 0 || opts.Duration <= 0 {
		return fail(base, 2, fmt.Errorf("duration and pps must be positive"))
	}
	if _, rejected := validateInjected(base, opts.InjectCandidate); rejected != nil {
		return *rejected
	}

	server, err := signal.NewServer(signal.ServerConfig{HeartbeatTimeout: signal.DefaultHeartbeatTimeout})
	if err != nil {
		return fail(base, 3, fmt.Errorf("start signal server: %w", err))
	}
	defer server.Close()
	offerer, err := peer.NewLoopback(peer.Config{ConnectTimeout: opts.Timeout})
	if err != nil {
		return fail(base, 4, fmt.Errorf("create offerer: %w", err))
	}
	defer offerer.Close()
	answerer, err := peer.NewLoopback(peer.Config{ConnectTimeout: opts.Timeout})
	if err != nil {
		return fail(base, 4, fmt.Errorf("create answerer: %w", err))
	}
	defer answerer.Close()
	if err := offerer.AddAudioTrack(0); err != nil {
		return fail(base, 4, fmt.Errorf("add audio track: %w", err))
	}
	leftProbe, err := newLoopbackProbe()
	if err != nil {
		return fail(base, 4, fmt.Errorf("create offer probe candidate: %w", err))
	}
	defer leftProbe.Close()
	rightProbe, err := newLoopbackProbe()
	if err != nil {
		return fail(base, 4, fmt.Errorf("create answer probe candidate: %w", err))
	}
	defer rightProbe.Close()

	caller := signal.NewClient(signal.ClientConfig{
		URL: server.URL(), Stream: "commentary", Role: "caller", Reconnect: true,
	})
	callee := signal.NewClient(signal.ClientConfig{
		URL: server.URL(), Stream: "commentary", Role: "callee", Reconnect: true,
	})
	defer caller.Close()
	defer callee.Close()
	connectCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	caller.Start()
	callee.Start()
	if err := caller.WaitReady(connectCtx); err != nil {
		return fail(base, 3, fmt.Errorf("caller handshake: %w", err))
	}
	if err := callee.WaitReady(connectCtx); err != nil {
		return fail(base, 3, fmt.Errorf("callee handshake: %w", err))
	}
	neg, err := negotiate(connectCtx, offerer, answerer, caller, callee, leftProbe, rightProbe)
	if err != nil {
		return fail(base, 3, err)
	}
	if err := waitConnected(connectCtx, offerer, answerer); err != nil {
		return fail(base, 4, err)
	}
	answerTrack, err := answerer.WaitRemoteTrack(connectCtx)
	if err != nil {
		return fail(base, 4, err)
	}
	selected, err := offerer.SelectedPair()
	if err != nil {
		return fail(base, 4, fmt.Errorf("selected candidate pair: %w", err))
	}
	if !loopice.IsLoopback(selected.Local.Address) || !loopice.IsLoopback(selected.Remote.Address) {
		return fail(base, 4, fmt.Errorf("selected pair is not loopback: %s -> %s", selected.Local, selected.Remote))
	}

	left := offerer.GatherReport()
	right := answerer.GatherReport()
	leftCandidates := append(append([]loopice.Candidate(nil), left.Accepted...), leftProbe.candidate)
	rightCandidates := append(append([]loopice.Candidate(nil), right.Accepted...), rightProbe.candidate)
	base.Gathered = left.Gathered + right.Gathered + 2
	base.Accepted = len(leftCandidates) + len(rightCandidates)
	base.Rejected = len(left.Rejected) + len(right.Rejected)
	base.RejectedCandidates = base.Rejected
	if base.Rejected > 0 {
		base.RejectedAddresses = append(append([]string(nil), left.Rejected...), right.Rejected...)
	}
	pairs := loopice.EnumeratePairs(leftCandidates, rightCandidates)
	base.PairsEvaluated = len(pairs)
	base.ProbeCandidates = 2
	base.CandidatePairs = make([]report.CandidatePairReport, 0, len(pairs))
	for _, pair := range pairs {
		base.CandidatePairs = append(base.CandidatePairs, report.CandidatePairReport{
			Local: pair.Local.String(), Remote: pair.Remote.String(),
		})
	}
	base.PairsSucceeded = 1
	base.SelectedPair = &report.SelectedPair{
		Local: selected.Local.String(), Remote: selected.Remote.String(),
		LocalAddress: selected.Local.Address, RemoteAddress: selected.Remote.Address,
		LocalPort: selected.Local.Port, RemotePort: selected.Remote.Port,
	}
	base.DTLSFingerprint = sdpFingerprint(neg.offerSDP)
	base.SignalVersion = signal.ProtocolVersion
	base.SignalTrickles = neg.trickleTX
	return runDatapath(ctx, opts, base, offerer.Track, answerTrack)
}

func negotiate(ctx context.Context, offerer, answerer *peer.Peer, caller, callee *signal.Client,
	leftProbe, rightProbe *loopbackProbe) (negotiation, error) {
	var result negotiation
	var candidateMu sync.Mutex
	var offerCandidates, answerCandidates []webrtc.ICECandidateInit
	offerer.SetCandidateHandler(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		candidateMu.Lock()
		offerCandidates = append(offerCandidates, init)
		candidateMu.Unlock()
	})
	answerer.SetCandidateHandler(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		candidateMu.Lock()
		answerCandidates = append(answerCandidates, init)
		candidateMu.Unlock()
	})
	flowErr := make(chan error, 8)
	reportFlowErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case flowErr <- err:
		default:
		}
	}
	answerReady := make(chan struct{}, 1)
	answerReceived := make(chan struct{}, 1)
	callee.OnOffer(func(offer signal.Offer) {
		if err := answerer.PC.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer, SDP: offer.SDP,
		}); err != nil {
			reportFlowErr(fmt.Errorf("apply offer: %w", err))
			return
		}
		answer, err := answerer.PC.CreateAnswer(nil)
		if err != nil {
			reportFlowErr(fmt.Errorf("create answer: %w", err))
			return
		}
		if err := answerer.PC.SetLocalDescription(answer); err != nil {
			reportFlowErr(fmt.Errorf("set answer: %w", err))
			return
		}
		answerReady <- struct{}{}
	})
	caller.OnAnswer(func(answer signal.Answer) {
		if err := offerer.PC.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer, SDP: answer.SDP,
		}); err != nil {
			reportFlowErr(fmt.Errorf("apply answer: %w", err))
			return
		}
		answerReceived <- struct{}{}
	})
	caller.OnTrickle(func(trickle signal.Trickle) {
		applyTrickle(offerer, trickle, reportFlowErr)
	})
	callee.OnTrickle(func(trickle signal.Trickle) {
		applyTrickle(answerer, trickle, reportFlowErr)
	})

	offer, err := offerer.PC.CreateOffer(nil)
	if err != nil {
		return result, fmt.Errorf("create offer: %w", err)
	}
	if err := offerer.PC.SetLocalDescription(offer); err != nil {
		return result, fmt.Errorf("set offer: %w", err)
	}
	result.offerSDP = offerer.PC.LocalDescription().SDP
	if err := offerer.WaitGathering(ctx); err != nil {
		return result, err
	}
	result.offerSDP = addProbeCandidate(result.offerSDP, leftProbe.sdpLine)
	if err := caller.Send(signal.TypeOffer, signal.Offer{SDP: result.offerSDP}); err != nil {
		return result, fmt.Errorf("send offer: %w", err)
	}
	if err := waitFlow(ctx, answerReady, flowErr); err != nil {
		return result, fmt.Errorf("answer: %w", err)
	}
	if err := answerer.WaitGathering(ctx); err != nil {
		return result, err
	}
	result.answerSDP = answerer.PC.LocalDescription().SDP
	result.answerSDP = addProbeCandidate(result.answerSDP, rightProbe.sdpLine)
	if err := callee.Send(signal.TypeAnswer, signal.Answer{SDP: result.answerSDP}); err != nil {
		return result, fmt.Errorf("send answer: %w", err)
	}
	candidateMu.Lock()
	leftCandidates := append([]webrtc.ICECandidateInit(nil), offerCandidates...)
	rightCandidates := append([]webrtc.ICECandidateInit(nil), answerCandidates...)
	candidateMu.Unlock()
	if err := sendTrickles(caller, leftCandidates); err != nil {
		return result, err
	}
	if err := sendTrickles(callee, rightCandidates); err != nil {
		return result, err
	}
	result.trickleTX = uint64(len(leftCandidates) + len(rightCandidates))
	if err := waitFlow(ctx, answerReceived, flowErr); err != nil {
		return result, fmt.Errorf("apply answer: %w", err)
	}
	return result, nil
}

func applyTrickle(p *peer.Peer, trickle signal.Trickle, reportErr func(error)) {
	if trickle.End || trickle.Candidate == nil {
		return
	}
	if err := p.PC.AddICECandidate(*trickle.Candidate); err != nil {
		reportErr(fmt.Errorf("add trickle candidate: %w", err))
	}
}

func sendTrickles(client *signal.Client, candidates []webrtc.ICECandidateInit) error {
	for i := range candidates {
		candidate := candidates[i]
		if err := client.Send(signal.TypeTrickle, signal.Trickle{Candidate: &candidate}); err != nil {
			return fmt.Errorf("send trickle: %w", err)
		}
	}
	return client.Send(signal.TypeTrickle, signal.Trickle{End: true})
}

func waitFlow(ctx context.Context, ready <-chan struct{}, errs <-chan error) error {
	select {
	case <-ready:
		return nil
	case err := <-errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitConnected(ctx context.Context, peers ...*peer.Peer) error {
	for _, item := range peers {
		if err := item.WaitConnected(ctx); err != nil {
			return err
		}
	}
	return nil
}
