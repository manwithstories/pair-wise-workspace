// Package verify orchestrates the terminal acceptance workflow.
package verify

import (
	"fmt"
	"time"
)

const (
	ExitOK            = 0
	ExitBadArgs       = 2
	ExitUnrecoverable = 3
	ExitInvariant     = 4
)

// Config are the accepted `mediapack verify` parameters.
type Config struct {
	Seed          int64
	Faults        []string
	BufferPackets int
	ConsumerDelay time.Duration
	CrashAfter    int
	Resume        bool
	WorkDir       string
	SilenceMS     int
}

// ExitError carries the required terminal exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("verify exit %d: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("verify exit %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

func badArgs(format string, args ...any) error {
	return &ExitError{Code: ExitBadArgs, Err: fmt.Errorf(format, args...)}
}

func unrecoverable(format string, args ...any) error {
	return &ExitError{Code: ExitUnrecoverable, Err: fmt.Errorf(format, args...)}
}

func invariant(format string, args ...any) error {
	return &ExitError{Code: ExitInvariant, Err: fmt.Errorf(format, args...)}
}

// ClipResult is the per-clip terminal data.
type ClipResult struct {
	ID      string
	SHA256  string
	FP      string
	Silence int
	Markers int
}

// Result contains all acceptance metrics.
type Result struct {
	WorkDir            string
	NormalHashes       []string
	ResumeHashes       []string
	Clips              []ClipResult
	Roundtrip          float64
	KeyframeHit        float64
	HashRepeat         float64
	PeakHeapMiB        float64
	BadCRC             int
	TruncatedCommitted int
	WALReplayed        int
	QueueHighWater     int
	DuplicateSources   int
}

// AllowedFaults lists the accepted fault injection names.
var AllowedFaults = []string{"pcr-wrap", "cc-gap", "psi-update", "bad-crc", "truncated"}

// Validate checks parameter ranges and fault names.
func Validate(cfg Config) error {
	if cfg.Seed < 0 {
		return badArgs("--seed must be non-negative")
	}
	if cfg.BufferPackets < 1 {
		return badArgs("--buffer-packets must be >= 1")
	}
	if cfg.CrashAfter < 0 {
		return badArgs("--crash-after must be >= 0")
	}
	if cfg.SilenceMS <= 0 {
		return badArgs("--silence-ms must be > 0")
	}
	allowed := map[string]bool{}
	for _, f := range AllowedFaults {
		allowed[f] = true
	}
	for _, f := range cfg.Faults {
		if !allowed[f] {
			return badArgs("unknown fault %q; allowed %v", f, AllowedFaults)
		}
	}
	return nil
}
