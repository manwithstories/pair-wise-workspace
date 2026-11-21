// Package driver wires the tsbatch library into a runnable batch
// pipeline: input reader -> TS/PSI parser -> unified clock -> structure
// analyzer -> bounded worker pool -> deterministic merger -> repack
// writer. The terminal entry point (cmd/tsbatch) only forwards
// arguments and prints verification data.
package driver

// Main dispatches a subcommand and returns the process exit code.
func Main(args []string) int {
	if len(args) < 2 {
		usage()
		return ExitArgs
	}
	switch args[1] {
	case "check":
		return Check(args[2:])
	case "repack":
		return Repack(args[2:])
	case "demo":
		return Demo(args[2:])
	default:
		usage()
		return ExitArgs
	}
}
