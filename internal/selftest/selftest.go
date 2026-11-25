// Package selftest generates local fixtures and drives the full medpipe
// pipeline end to end so the terminal adapter can verify every subsystem
// without any external service or pre-existing media files.
package selftest

// Options configures a self-test run.
type Options struct {
	// Dir is the working directory for fixtures, checkpoints and outputs.
	Dir string
	// Seed makes fixture generation deterministic.
	Seed int64
	// Resume triggers an interrupted-run-and-restart scenario.
	Resume bool
}

// Run executes the self-test. The full fixture and pipeline behavior is
// added in subsequent batches; this entry point validates options first.
func Run(opts Options) error {
	return runAll(opts)
}
