package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"relayctl/internal/relay"
)

type sinkReport struct {
	Schema     string `json:"schema"`
	Bind       string `json:"bind"`
	Packets    uint64 `json:"packets"`
	Bytes      uint64 `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

func runSink(args []string, stdout io.Writer) error {
	fs := newFlagSet("sink", stdout)
	bind := fs.String("bind", "127.0.0.1:0", "loopback UDP sink bind address")
	duration := fs.Duration("duration", 10*time.Second, "maximum sink run time")
	expectPackets := fs.Uint64("expect-packets", 0, "stop after this many packets (0 means duration only)")
	jsonPath := fs.String("json", "", "JSON report path")
	if err := fs.Parse(args); err != nil {
		return withCode("bad_args", "see relayctl sink -h", err)
	}
	if fs.NArg() != 0 {
		return withCode("bad_args", "unexpected sink arguments", errors.New(fs.Args()[0]))
	}
	if err := validateLoopbackAddr(*bind); err != nil {
		return err
	}
	if *duration <= 0 {
		return withCode("bad_args", "duration must be positive", errors.New("duration <= 0"))
	}
	sink, err := relay.ListenSink(*bind)
	if err != nil {
		return fmt.Errorf("start sink: %w", err)
	}
	defer sink.Close()
	done := make(chan struct{})
	runErr := make(chan error, 1)
	go func() { runErr <- sink.Run(done) }()
	start := time.Now()
	timer := time.NewTimer(*duration)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-timer.C:
			break loop
		case <-ticker.C:
			packets, _ := sink.Counts()
			if *expectPackets > 0 && packets >= *expectPackets {
				break loop
			}
		}
	}
	close(done)
	_ = sink.Close()
	if err := <-runErr; err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	packets, bytes := sink.Counts()
	report := sinkReport{Schema: "relayctl.sink.v1", Bind: sink.Addr(), Packets: packets, Bytes: bytes, DurationMS: time.Since(start).Milliseconds()}
	if err := writeJSON(*jsonPath, report); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "sink=%s sink_packets=%d sink_bytes=%d\n", report.Bind, report.Packets, report.Bytes)
	if *expectPackets > 0 && packets < *expectPackets {
		return withCode("sink_timeout", "increase --duration or verify upstream forwarding", fmt.Errorf("wanted %d packets, got %d", *expectPackets, packets))
	}
	return nil
}
