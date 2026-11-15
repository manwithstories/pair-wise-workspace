package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/media"
	"pairwise.local/relayctl/internal/relay"
)

func runPublish(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("publish", stderr)
	address := flags.String("signal", "127.0.0.1:47000", "loopback signaling address")
	room := flags.String("room", "demo", "signaling room")
	duration := flags.Duration("duration", 10*time.Second, "publish duration")
	if !parseFlags(flags, args) || *duration <= 0 || *room == "" {
		return exitConfig
	}
	if err := guard.ValidateListenAddress(*address); err != nil {
		fmt.Fprintf(stderr, "refusing signaling address: %v\n", err)
		return codeForError(err)
	}
	api, err := relay.NewLoopbackAPI()
	if err != nil {
		fmt.Fprintf(stderr, "create WebRTC API: %v\n", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration+iceTimeout)
	defer cancel()
	publisher, err := dialPublisher(ctx, api, *address, *room)
	if err != nil {
		fmt.Fprintf(stderr, "publish setup: %v\n", err)
		return codeForError(err)
	}
	defer publisher.Close()
	source := media.NewSource(media.Config{SSRC: 0xBEEF, Seed: 77})
	var sent atomic.Uint64
	sourceCtx, stop := context.WithTimeout(ctx, *duration+time.Second)
	defer stop()
	err = source.RunCount(sourceCtx, media.PacketCount(*duration, media.DefaultFrameDuration), func(packet *rtp.Packet) error {
		if err := publisher.WriteRTP(packet); err != nil {
			return err
		}
		sent.Add(1)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "publish media: %v\n", err)
		return exitSignal
	}
	writeJSONLine(stdout, map[string]uint64{"published_packets": sent.Load()})
	return exitOK
}

func runSubscribe(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("subscribe", stderr)
	address := flags.String("signal", "127.0.0.1:47000", "loopback signaling address")
	room := flags.String("room", "demo", "signaling room")
	duration := flags.Duration("duration", 10*time.Second, "subscribe duration")
	if !parseFlags(flags, args) || *duration <= 0 || *room == "" {
		return exitConfig
	}
	if err := guard.ValidateListenAddress(*address); err != nil {
		fmt.Fprintf(stderr, "refusing signaling address: %v\n", err)
		return codeForError(err)
	}
	api, err := relay.NewLoopbackAPI()
	if err != nil {
		fmt.Fprintf(stderr, "create WebRTC API: %v\n", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration+iceTimeout)
	defer cancel()
	subscriber, err := dialSubscriber(ctx, api, *address, *room)
	if err != nil {
		fmt.Fprintf(stderr, "subscribe setup: %v\n", err)
		return codeForError(err)
	}
	defer subscriber.Close()
	select {
	case <-time.After(*duration):
	case <-ctx.Done():
	}
	writeJSONLine(stdout, map[string]uint64{"subscribed_packets": subscriber.packets.Load()})
	return exitOK
}

func writeJSONLine(writer io.Writer, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(writer, "%s\n", data)
}
