package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/pion/rtp"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/impair"
	"pairwise.local/relayctl/internal/media"
	"pairwise.local/relayctl/internal/relay"
	reportpkg "pairwise.local/relayctl/internal/report"
	"pairwise.local/relayctl/internal/signal"
)

func runVerify(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify", stderr)
	signalAddress := flags.String("signal", "127.0.0.1:47000", "loopback signaling address")
	duration := flags.Duration("duration", 12*time.Second, "media duration")
	egressCount := flags.Int("egress", 3, "number of subscriber outlets (1-8)")
	impairSpec := flags.String("impair", "", "delay=20ms,reorder=5,loss=0.01,duplicate=0,seed=7")
	maxP95 := flags.Float64("max-p95-jitter", 15, "maximum aggregate p95 jitter in ms")
	maxLoss := flags.Float64("max-loss", 0.05, "maximum drop ratio (0-1)")
	outPath := flags.String("out", "out/report.json", "report JSON output path")
	room := flags.String("room", "verify", "signaling room")
	if !parseFlags(flags, args) {
		return exitConfig
	}
	if *duration <= 0 || *egressCount < 1 || *egressCount > relay.MaxEgress ||
		*maxP95 < 0 || *maxLoss < 0 || *maxLoss > 1 || *room == "" {
		fmt.Fprintln(stderr, "invalid verify parameters")
		return exitConfig
	}
	if err := guard.ValidateListenAddress(*signalAddress); err != nil {
		fmt.Fprintf(stderr, "refusing signaling address: %v\n", err)
		return codeForError(err)
	}
	impairConfig, err := impair.ParseSpec(*impairSpec)
	if err != nil {
		fmt.Fprintf(stderr, "invalid impairment: %v\n", err)
		return exitConfig
	}
	engineConfig := relay.DefaultConfig()
	engineConfig.Impair = impairConfig
	engine, err := relay.NewEngine(engineConfig)
	if err != nil {
		fmt.Fprintf(stderr, "create relay engine: %v\n", err)
		return exitConfig
	}
	defer engine.Close()
	server := signal.NewServer(engine)
	if err := server.Listen(*signalAddress); err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return codeForError(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx) }()
	dialAddress := server.Addr().String()

	api, err := relay.NewLoopbackAPI()
	if err != nil {
		fmt.Fprintf(stderr, "create WebRTC API: %v\n", err)
		return exitConfig
	}
	publisher, err := dialPublisher(ctx, api, dialAddress, *room)
	if err != nil {
		fmt.Fprintf(stderr, "publisher setup: %v\n", err)
		return setupExitCode(server, serveErr, err)
	}
	defer publisher.Close()
	subscribers := make([]*mediaSession, 0, *egressCount)
	for i := 0; i < *egressCount; i++ {
		subscriber, err := dialSubscriber(ctx, api, dialAddress, *room)
		if err != nil {
			fmt.Fprintf(stderr, "subscriber %d setup: %v\n", i+1, err)
			for _, item := range subscribers {
				item.Close()
			}
			return setupExitCode(server, serveErr, err)
		}
		subscribers = append(subscribers, subscriber)
		defer subscriber.Close()
	}
	if err := waitCandidatePairs(ctx, engine, *egressCount+1, iceTimeout); err != nil {
		fmt.Fprintf(stderr, "ICE readiness: %v\n", err)
		return exitSignal
	}

	source := media.NewSource(media.Config{SSRC: 0xA11CE, Seed: 99})
	sourceCtx, stopSource := context.WithTimeout(ctx, *duration+time.Second)
	sourceDone := make(chan error, 1)
	packetCount := media.PacketCount(*duration, media.DefaultFrameDuration)
	go func() {
		sourceDone <- source.RunCount(sourceCtx, packetCount, func(packet *rtp.Packet) error {
			return publisher.WriteRTP(packet)
		})
	}()
	sourceErr := <-sourceDone
	stopSource()
	if sourceErr != nil {
		fmt.Fprintf(stderr, "source stopped early: %v\n", sourceErr)
	}
	var runtimeErr error
	select {
	case fatal := <-server.Fatal():
		runtimeErr = fatal
	case serveFailure := <-serveErr:
		if serveFailure != nil && ctx.Err() == nil {
			runtimeErr = serveFailure
		}
	default:
	}
	time.Sleep(200 * time.Millisecond)
	engine.Drain()
	result := reportpkg.Build(engine.Snapshot(), server.Stats())
	var received uint64
	subscriberEmpty := false
	for _, subscriber := range subscribers {
		count := subscriber.packets.Load()
		received += count
		if result.ForwardedPackets > 0 && count == 0 {
			subscriberEmpty = true
		}
	}
	if err := reportpkg.WriteLine(stdout, result); err != nil {
		fmt.Fprintf(stderr, "write stdout report: %v\n", err)
		return exitConfig
	}
	if *outPath != "" {
		if err := reportpkg.WriteFile(*outPath, result); err != nil {
			fmt.Fprintf(stderr, "write report file: %v\n", err)
			return exitConfig
		}
	}
	if runtimeErr != nil {
		fmt.Fprintf(stderr, "verification failed during media run: %v\n", runtimeErr)
		return codeForError(runtimeErr)
	}
	if sourceErr != nil {
		return codeForError(sourceErr)
	}
	if subscriberEmpty {
		fmt.Fprintln(stderr, "verification failed: a subscriber received zero RTP packets")
		return exitSignal
	}
	packets := result.ForwardedPackets + result.DroppedPackets
	lossRatio := 0.0
	if packets > 0 {
		lossRatio = float64(result.DroppedPackets) / float64(packets)
	}
	if result.JitterMS.P95 > *maxP95 {
		fmt.Fprintf(stderr, "verification failed: p95 jitter %.3fms > %.3fms\n", result.JitterMS.P95, *maxP95)
		return exitThreshold
	}
	if lossRatio > *maxLoss {
		fmt.Fprintf(stderr, "verification failed: drop ratio %.4f > %.4f\n", lossRatio, *maxLoss)
		return exitThreshold
	}
	if result.EgressFailed > 0 {
		fmt.Fprintf(stderr, "verification failed: %d egress failed\n", result.EgressFailed)
		return exitThreshold
	}
	fmt.Fprintf(stderr, "verification passed: %d forwarded, %d subscriber-received, %.3fms p95 jitter, %.4f drop ratio\n",
		result.ForwardedPackets, received, result.JitterMS.P95, lossRatio)
	return exitOK
}

func waitCandidatePairs(ctx context.Context, engine *relay.Engine, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if engine.Snapshot().CandidatePairs >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d/%d candidate pairs selected", engine.Snapshot().CandidatePairs, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func setupExitCode(server *signal.Server, serveErr <-chan error, err error) int {
	select {
	case fatal := <-server.Fatal():
		return codeForError(fatal)
	case serveFailure := <-serveErr:
		if serveFailure == nil {
			return exitSignal
		}
		return codeForError(serveFailure)
	default:
		return codeForError(err)
	}
}
