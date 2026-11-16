// Package cli defines the relay command's flags, exit codes and the shared
// wiring between the demo, serve and probe subcommands.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/fixture"
	rtpx "github.com/manwithstories/pair-wise-workspace/internal/rtp"
)

// Exit codes are part of the command contract; scripts branch on them.
const (
	ExitOK          = 0
	ExitUsage       = 2
	ExitTransport   = 3
	ExitThreshold   = 4
	ExitInterrupted = 130
)

// ErrUsage wraps flag and argument problems so main can map them to exit 2.
var ErrUsage = errors.New("invalid arguments")

// Options is the fully parsed configuration for one run.
type Options struct {
	Command string

	Addr          string
	Packets       int
	IntervalMs    int
	Reorder       int
	DuplicateRate float64
	LossRate      float64
	ReorderRate   float64
	Seed          int64
	Queue         int
	ConnectMs     int
	MediaMs       int
	DrainMs       int
	IdleMs        int
	SSRCVar       uint
	SSRC          uint32
	JSON          bool
	Quiet         bool
	ProbeAddr     string // probe against an existing server instead of self-hosting
	ProbePackets  int

	// thresholds for the demo's post-run check
	ExpectMinLost int64
	ExpectMaxLost int64
}

// Parse turns argv (without the program name) into Options.
//
// Every subcommand shares the media flags so scripts can add --json to any of
// them; flags that a subcommand does not use are still accepted so a shared
// invocation does not fail on an irrelevant option.
func Parse(args []string, stderr io.Writer) (*Options, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: missing subcommand (want demo, serve or probe)", ErrUsage)
	}
	opts := &Options{Command: args[0], Addr: "127.0.0.1:0", Queue: 256, SSRCVar: 0xC0FFEE01, SSRC: 0xC0FFEE01}
	fs := flag.NewFlagSet("relay "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	fs.StringVar(&opts.Addr, "addr", opts.Addr, "signaling listen/target address")
	fs.IntVar(&opts.Packets, "packets", 1000, "number of source RTP packets")
	fs.IntVar(&opts.IntervalMs, "interval-ms", 5, "milliseconds between packets")
	fs.IntVar(&opts.Reorder, "reorder", rtpx.DefaultReorderWindow, "reorder window in packets")
	fs.Float64Var(&opts.DuplicateRate, "duplicate-rate", 0, "fraction of packets duplicated (0..1)")
	fs.Float64Var(&opts.LossRate, "loss-rate", 0, "fraction of packets dropped at the sender (0..1)")
	fs.Float64Var(&opts.ReorderRate, "reorder-rate", 0.05, "fraction of packets delayed within the reorder window (0..1)")
	fs.Int64Var(&opts.Seed, "seed", 1, "deterministic fixture seed")
	fs.IntVar(&opts.Queue, "queue", opts.Queue, "forwarder queue capacity")
	fs.IntVar(&opts.ConnectMs, "connect-ms", 10000, "ICE/DTLS connect timeout")
	fs.IntVar(&opts.MediaMs, "media-ms", 5000, "remote track / media wait timeout")
	fs.IntVar(&opts.DrainMs, "drain-ms", 500, "quiet period before reading final metrics")
	fs.IntVar(&opts.IdleMs, "idle-ms", 5000, "signaling session idle timeout")
	fs.UintVar(&opts.SSRCVar, "ssrc", opts.SSRCVar, "fixture SSRC")
	fs.BoolVar(&opts.JSON, "json", false, "emit machine-readable JSON")
	fs.BoolVar(&opts.Quiet, "quiet", false, "suppress the human-readable summary")
	fs.StringVar(&opts.ProbeAddr, "probe-addr", "", "probe: signaling address to check")
	fs.IntVar(&opts.ProbePackets, "probe-packets", 8, "probe: packets to send through the self-check")

	if err := fs.Parse(args[1:]); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: unexpected arguments %v", ErrUsage, fs.Args())
	}
	opts.SSRC = uint32(opts.SSRCVar)
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return opts, nil
}

func (o *Options) validate() error {
	switch o.Command {
	case "demo", "serve", "probe":
	default:
		return fmt.Errorf("%w: unknown subcommand %q (want demo, serve or probe)", ErrUsage, o.Command)
	}
	if o.Packets <= 0 || o.Packets > 2_000_000 {
		return fmt.Errorf("%w: --packets must be in [1,2000000]", ErrUsage)
	}
	if o.IntervalMs <= 0 || o.IntervalMs > 10_000 {
		return fmt.Errorf("%w: --interval-ms must be in [1,10000]", ErrUsage)
	}
	if o.Reorder < 0 || o.Reorder > 4096 {
		return fmt.Errorf("%w: --reorder must be in [0,4096]", ErrUsage)
	}
	if o.Queue <= 0 || o.Queue > 1_000_000 {
		return fmt.Errorf("%w: --queue must be in [1,1000000]", ErrUsage)
	}
	for name, v := range map[string]float64{
		"--loss-rate":      o.LossRate,
		"--duplicate-rate": o.DuplicateRate,
		"--reorder-rate":   o.ReorderRate,
	} {
		if v < 0 || v > 1 {
			return fmt.Errorf("%w: %s must be in [0,1]", ErrUsage, name)
		}
	}
	if o.ProbeAddr != "" && o.Command == "probe" {
		o.Addr = o.ProbeAddr
	}
	if o.ConnectMs <= 0 || o.MediaMs <= 0 || o.DrainMs < 0 || o.IdleMs <= 0 {
		return fmt.Errorf("%w: timeouts must be positive", ErrUsage)
	}
	return nil
}

// ConnectTimeout is the parsed ICE/DTLS timeout.
func (o *Options) ConnectTimeout() time.Duration {
	return time.Duration(o.ConnectMs) * time.Millisecond
}

// MediaTimeout bounds waiting for the remote track.
func (o *Options) MediaTimeout() time.Duration { return time.Duration(o.MediaMs) * time.Millisecond }

// Drain is the quiet period that lets in-flight packets land before metrics
// are read.
func (o *Options) Drain() time.Duration { return time.Duration(o.DrainMs) * time.Millisecond }

// Idle is the signaling session idle timeout.
func (o *Options) Idle() time.Duration { return time.Duration(o.IdleMs) * time.Millisecond }

// FixtureConfig maps the command flags onto a generator configuration.
func (o *Options) FixtureConfig() fixture.Config {
	return fixture.Config{
		SSRC:          o.SSRC,
		Packets:       o.Packets,
		IntervalMs:    o.IntervalMs,
		ReorderWindow: o.Reorder,
		ReorderRate:   o.ReorderRate,
		DuplicateRate: o.DuplicateRate,
		LossRate:      o.LossRate,
		Seed:          o.Seed,
		StartSequence: fixture.DefaultStartSequence,
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `relay — loopback WebRTC RTP data-path harness

Usage:
  relay demo  [flags]     run one fixture through two loopback peers and report
  relay serve [flags]     run only the local signaling server
  relay probe [flags]     exchange a tiny fixture through the signaling server

Common flags:
  --addr --packets --interval-ms --reorder --duplicate-rate --loss-rate
  --reorder-rate --seed --queue --connect-ms --media-ms --drain-ms
  --idle-ms --ssrc --json --quiet

Exit codes: 0 ok, 2 invalid arguments, 3 transport (signaling/ICE/DTLS),
            4 metric thresholds not met.
`)
}
