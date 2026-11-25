package selftest

import "medpipe/internal/clock"

// runAll will be expanded with fixture generation and pipeline stages.
func runAll(opts Options) error {
	if opts.Dir == "" {
		return clock.E(clock.KindArgument, "selftest",
			clock.WithDetail("working directory is required"))
	}
	return nil
}
