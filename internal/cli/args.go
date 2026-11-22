// Package cli parses terminal arguments and maps structured pipeline errors to
// the documented process exit codes: 1 invalid args/config, 2 input/
// processing failure, 3 checkpoint mismatch, 0 success.
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
	"mediarun/internal/pipeline"
)

// Args holds the parsed `demo` subcommand options.
type Args struct {
	WorkDir        string
	Format         string // mixed | mp4 | mxf
	QueueDepth     int
	TargetBitrate  int64
	Inject         string
	InterruptAfter int64
	Resume         bool
}

// Defaults documented by the specification.
const (
	defaultQueueDepth = pipeline.DefaultQueueDepth // 8
	defaultBitrate    = int64(12_000_000)
)

// Parse converts raw os.Args (excluding the program name) into Args. The only
// supported verb is `demo`; anything else is an INVALID_ARGS error.
func Parse(argv []string) (*Args, error) {
	if len(argv) == 0 {
		return nil, mediaerr.New(mediaerr.CodeInvalidArgs, "missing subcommand (expected 'demo')")
	}
	if argv[0] != "demo" {
		return nil, mediaerr.New(mediaerr.CodeInvalidArgs,
			"unknown subcommand "+argv[0]+": only 'demo' is supported")
	}
	a := &Args{
		Format: "mixed", QueueDepth: defaultQueueDepth, TargetBitrate: defaultBitrate,
	}
	flags := map[string]bool{
		"--work": true, "--format": true, "--queue-depth": true, "--target-bitrate": true,
		"--inject": true, "--interrupt-after": true,
	}
	boolFlags := map[string]bool{"--resume": true}
	for i := 1; i < len(argv); i++ {
		token := argv[i]
		key, value, hasValue := splitToken(token)
		if boolFlags[key] {
			if hasValue {
				return nil, mediaerr.New(mediaerr.CodeInvalidArgs, key+" is a boolean flag")
			}
			a.Resume = true
			continue
		}
		if !flags[key] {
			return nil, mediaerr.New(mediaerr.CodeInvalidArgs, "unknown flag "+key)
		}
		if !hasValue {
			i++
			if i >= len(argv) {
				return nil, mediaerr.New(mediaerr.CodeInvalidArgs, "flag "+key+" needs a value")
			}
			value = argv[i]
		}
		if err := a.assign(key, value); err != nil {
			return nil, err
		}
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return a, nil
}

func splitToken(token string) (key, value string, hasValue bool) {
	if i := strings.Index(token, "="); i >= 0 {
		return token[:i], token[i+1:], true
	}
	return token, "", false
}

func (a *Args) assign(key, value string) error {
	switch key {
	case "--work":
		a.WorkDir = value
	case "--format":
		a.Format = value
	case "--inject":
		a.Inject = value
	case "--queue-depth":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return mediaerr.New(mediaerr.CodeInvalidArgs,
				"--queue-depth must be a positive integer, got "+value)
		}
		a.QueueDepth = n
	case "--target-bitrate":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			return mediaerr.New(mediaerr.CodeInvalidArgs,
				"--target-bitrate must be positive, got "+value)
		}
		a.TargetBitrate = n
	case "--interrupt-after":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return mediaerr.New(mediaerr.CodeInvalidArgs,
				"--interrupt-after must be >= 0, got "+value)
		}
		a.InterruptAfter = n
	default:
		return mediaerr.New(mediaerr.CodeInvalidArgs, "unhandled flag "+key)
	}
	return nil
}

func (a *Args) validate() error {
	if a.WorkDir == "" {
		return mediaerr.New(mediaerr.CodeInvalidArgs, "--work is required")
	}
	switch a.Format {
	case "mixed", "mp4", "mxf":
	default:
		return mediaerr.New(mediaerr.CodeInvalidArgs,
			"--format must be one of mixed|mp4|mxf, got "+a.Format)
	}
	if a.Inject != "" {
		switch a.Inject {
		case "truncated-box", "truncated-klv", "bad-magic":
		default:
			return mediaerr.New(mediaerr.CodeInvalidArgs,
				"unsupported --inject value "+a.Inject)
		}
	}
	return nil
}

// Mapping builds the explicit track mapping for the demo's two-track input.
func (a *Args) Mapping() mapping.Config {
	return mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/1"},
	}}
}

// ExitCode is a thin wrapper so the main package has one place to consult.
func ExitCode(err error) int { return mediaerr.ExitCode(err) }

// Usage returns the terminal help text.
func Usage() string {
	return strings.TrimSpace(fmt.Sprintf(`
mediarun demo --work DIR [--format mixed|mp4|mxf] [--queue-depth N]
             [--target-bitrate BPS] [--interrupt-after N] [--resume]
             [--inject truncated-box|truncated-klv|bad-magic]

exit codes: 0 ok, 1 invalid args/config, 2 input/processing failure,
            3 checkpoint mismatch
default queue-depth=%d target-bitrate=%d
`, defaultQueueDepth, defaultBitrate))
}
