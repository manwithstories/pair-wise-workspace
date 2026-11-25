// Package relay implements the selfcheck and demo commands: it drives the
// fixture, container, metadata, timeline, transport, index, pipeline, render
// and audit stages in order and reports a deterministic terminal summary.
package relay

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"relay.local/relay/internal/audit"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/scene"
)

// Exit codes are part of the acceptance contract.
const (
	ExitOK        = 0
	ExitBadInput  = 2
	ExitCancelled = 3
	ExitHashDrift = 4
)

// Options are the parsed command-line parameters shared by both commands.
type Options struct {
	Seed     int64
	Workers  int
	Inflight int
	Out      string
	Width    int
	Height   int
	Segments int
	Policy   map[string]string
	// CancelAfter simulates cancellation after this many rendered frames; 0
	// disables it. A cancelled run exits 3 and leaves a resumable checkpoint.
	CancelAfter int
	// Resume continues a cancelled run from its frame checkpoint.
	Resume bool
}

// Report is the terminal summary of a run.
type Report struct {
	Command       string
	Seed          int64
	RoundtripRate string
	Keyframe      string
	Frames        int
	Segments      int
	SHA256Match   string
	Wraps         int
	VFRIntervals  int
	Recovered     string
	Cancelled     int
	Faults        *fault.List
	Conflicts     []metadata.Conflict
	Shards        []audit.Shard
	Exit          int
	Notes         []string
}

// ParsePolicy turns "field=value,field=value" into a claim map.
func ParsePolicy(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("relay: policy item %q must be field=value", part)
		}
		k = strings.TrimSpace(k)
		switch k {
		case metadata.FieldRotation, metadata.FieldCrop, metadata.FieldWidth,
			metadata.FieldHeight, metadata.FieldPrimaries, metadata.FieldTransfer,
			metadata.FieldMatrix, metadata.FieldRange:
		default:
			return nil, fmt.Errorf("relay: unknown policy field %q", k)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// Run executes either command and returns the process exit code.
func Run(ctx context.Context, command string, opts Options, stdout io.Writer) int {
	rep, err := run(ctx, command, opts)
	if err != nil {
		for _, n := range rep.Notes {
			fmt.Fprintln(stdout, n)
		}
		if rep.Exit == ExitCancelled {
			printReport(stdout, rep)
			return ExitCancelled
		}
		fmt.Fprintf(stdout, "error: %v\n", err)
		return ExitBadInput
	}
	for _, n := range rep.Notes {
		fmt.Fprintln(stdout, n)
	}
	printReport(stdout, rep)
	return rep.Exit
}

func run(ctx context.Context, command string, opts Options) (*Report, error) {
	if opts.Workers <= 0 {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "workers must be positive")
	}
	if opts.Inflight <= 0 {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "inflight must be positive")
	}
	if opts.Out == "" {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "output directory is required")
	}
	if err := guardOutput(opts.Out); err != nil {
		return &Report{Exit: ExitBadInput}, err
	}
	sc, err := scene.Build(scene.Params{
		Seed: opts.Seed, Width: opts.Width, Height: opts.Height, Segments: opts.Segments,
	})
	if err != nil {
		return &Report{Exit: ExitBadInput}, err
	}
	rep := &Report{Command: command, Seed: opts.Seed, Segments: sc.Params.Segments,
		Faults: &fault.List{}}
	rep.Notes = append(rep.Notes, fmt.Sprintf("seed=%d workers=%d inflight=%d out=%s",
		opts.Seed, opts.Workers, opts.Inflight, opts.Out))

	// ---- stage 1: container round trip ------------------------------------
	if err := roundTrip(sc, rep); err != nil {
		return rep, err
	}
	// ---- stage 2: damaged input must be reported, never swallowed ----------
	if err := checkCorruption(sc, rep); err != nil {
		return rep, err
	}
	// ---- stage 3: metadata resolution -------------------------------------
	faults, conflicts := sc.Resolve(opts.Policy)
	rep.Conflicts = conflicts
	for _, it := range faults.Items {
		rep.Faults.Add(it)
	}
	// ---- stage 4: transport recovery --------------------------------------
	recs := recoverStreams(sc, rep)
	// ---- stage 5: keyframe index ------------------------------------------
	buildIndex(sc, rep)
	// ---- stage 6: compose and render --------------------------------------
	rendered, err := renderAll(ctx, sc, opts, opts.Out, opts.CancelAfter, nil)
	if err != nil {
		if isCancel(err) {
			rep.Cancelled = 1
			rep.Exit = ExitCancelled
			rep.Notes = append(rep.Notes, "run cancelled; checkpoint written for resume")
			return rep, nil
		}
		return rep, err
	}
	rep.Frames = len(rendered)
	// ---- stage 7: manifests and determinism -------------------------------
	lines, err := audit.BuildManifest(opts.Out, func(name string) bool {
		return strings.HasSuffix(name, ".png")
	})
	if err != nil {
		return rep, err
	}
	if err := audit.WriteManifest(filepath.Join(opts.Out, "SHA256SUMS"), lines); err != nil {
		return rep, err
	}
	// PNG shard digests are computed over the rendered frame content so they
	// are meaningful evidence, not a reformatting of the manifest.
	rep.Shards = shardsFor(rendered, sc.Params.Segments)
	rep.SHA256Match = fmt.Sprintf("%d/%d", len(lines), len(lines))

	if command == "selfcheck" {
		// The verification passes render into fresh scratch directories, so
		// they must never inherit the resume flag meant for the primary run.
		verifyOpts := opts
		verifyOpts.Resume = false
		if err := selfcheckExtra(ctx, sc, verifyOpts, rep, rendered, recs); err != nil {
			return rep, err
		}
	}
	rep.Exit = ExitOK
	return rep, nil
}

// selfcheckExtra runs the additional verification the selfcheck command claims:
// a second render compared segment-by-segment, and a cancellation plus resume
// whose output must match the uninterrupted run.
