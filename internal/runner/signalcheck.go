package runner

import (
	"context"
	"fmt"

	"rtprelay/internal/signal"
)

func RunSignal(ctx context.Context, raw Options) Result {
	opts := normalize(raw)
	base := baseReport(opts)
	base.Mode = "signal"
	server, err := signal.NewServer(signal.ServerConfig{HeartbeatTimeout: signal.DefaultHeartbeatTimeout})
	if err != nil {
		return fail(base, 3, err)
	}
	defer server.Close()
	caller := signal.NewClient(signal.ClientConfig{
		URL: server.URL(), Stream: "probe", Role: "caller", Reconnect: true,
	})
	callee := signal.NewClient(signal.ClientConfig{
		URL: server.URL(), Stream: "probe", Role: "callee", Reconnect: true,
	})
	defer caller.Close()
	defer callee.Close()
	offerCh := make(chan signal.Offer, 1)
	answerCh := make(chan signal.Answer, 1)
	flowErr := make(chan error, 4)
	callee.OnOffer(func(offer signal.Offer) { offerCh <- offer })
	caller.OnAnswer(func(answer signal.Answer) { answerCh <- answer })

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	caller.Start()
	callee.Start()
	if err := caller.WaitReady(ctx); err != nil {
		return fail(base, 3, err)
	}
	if err := callee.WaitReady(ctx); err != nil {
		return fail(base, 3, err)
	}
	offer := signal.Offer{SDP: "v=0\r\no=signal-check 1 1 IN IP4 127.0.0.1\r\n"}
	if err := caller.Send(signal.TypeOffer, offer); err != nil {
		return fail(base, 3, fmt.Errorf("send offer: %w", err))
	}
	select {
	case <-offerCh:
	case err := <-flowErr:
		return fail(base, 3, err)
	case <-ctx.Done():
		return fail(base, 3, ctx.Err())
	}
	if err := callee.Send(signal.TypeAnswer, signal.Answer{SDP: "v=0\r\no=signal-check 2 2 IN IP4 127.0.0.1\r\n"}); err != nil {
		return fail(base, 3, fmt.Errorf("send answer: %w", err))
	}
	select {
	case <-answerCh:
	case err := <-flowErr:
		return fail(base, 3, err)
	case <-ctx.Done():
		return fail(base, 3, ctx.Err())
	}
	if err := caller.Send(signal.TypeTrickle, signal.Trickle{End: true}); err != nil {
		return fail(base, 3, fmt.Errorf("send trickle: %w", err))
	}
	base.SignalVersion = signal.ProtocolVersion
	base.SignalTrickles = 1
	base.OK = true
	return Result{Report: base, ExitCode: 0}
}
