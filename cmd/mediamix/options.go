// Command mediamix is the terminal acceptance entry point. It generates
// deterministic RTP/RTCP fixtures, runs them through the full media
// pipeline, and prints per-case metrics. Exit codes encode the outcome.
package main

// Exit codes follow the acceptance contract.
const (
	exitOK             = 0
	exitInputError     = 2
	exitRollbackFailed = 3
	exitMetricsExceeded = 4
)

// options holds the parsed command-line flags.
type options struct {
	caseName        string
	seed            int64
	seconds         int
	loss            float64
	reorder         float64
	queueMiB        int
	slowConsumerMs  int
	fault           string
	outDir          string
}

func defaultOptions() options {
	return options{
		caseName:       "all",
		seed:           41,
		seconds:        12,
		loss:           0.02,
		reorder:        0.05,
		queueMiB:       8,
		slowConsumerMs: 0,
		fault:          "after-normalize",
		outDir:         "",
	}
}

// parseArgs is a small, dependency-free flag parser. It returns a
// locatable error (mapped to exit code 2) instead of exiting on its own.
func parseArgs(args []string) (options, error) {
	o := defaultOptions()
	if len(args) == 0 {
		return o, nil
	}
	if args[0] != "demo" {
		return o, usageError("only the 'demo' subcommand is supported")
	}
	rest := args[1:]
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(rest) {
			return "", usageError("flag " + flag + " requires a value")
		}
		return rest[i+1], nil
	}
	for i := 0; i < len(rest); i++ {
		flag := rest[i]
		switch flag {
		case "--case":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			o.caseName = v
			i++
		case "--seed":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			n, err := parseInt(v)
			if err != nil {
				return o, usageError("--seed must be an integer")
			}
			o.seed = int64(n)
			i++
		case "--seconds":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			n, err := parseInt(v)
			if err != nil {
				return o, usageError("--seconds must be an integer")
			}
			o.seconds = n
			i++
		case "--loss":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			f, err := parseFloat(v)
			if err != nil || f < 0 || f > 1 {
				return o, usageError("--loss must be within 0..1")
			}
			o.loss = f
			i++
		case "--reorder":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			f, err := parseFloat(v)
			if err != nil || f < 0 || f > 1 {
				return o, usageError("--reorder must be within 0..1")
			}
			o.reorder = f
			i++
		case "--queue-mib":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			n, err := parseInt(v)
			if err != nil || n <= 0 || n > 8 {
				return o, usageError("--queue-mib must be within 1..8")
			}
			o.queueMiB = n
			i++
		case "--slow-consumer-ms":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			n, err := parseInt(v)
			if err != nil || n < 0 {
				return o, usageError("--slow-consumer-ms must be >= 0")
			}
			o.slowConsumerMs = n
			i++
		case "--fault":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			o.fault = v
			i++
		case "--out":
			v, err := need(i, flag)
			if err != nil {
				return o, err
			}
			o.outDir = v
			i++
		default:
			return o, usageError("unknown flag: " + flag)
		}
	}
	if o.seconds <= 0 || o.seconds > 600 {
		return o, usageError("--seconds must be within 1..600 (10 minute cap)")
	}
	switch o.caseName {
	case "all", "normal", "corrupt", "interrupt":
	default:
		return o, usageError("--case must be one of all|normal|corrupt|interrupt")
	}
	switch o.fault {
	case "none", "after-normalize", "before-commit":
	default:
		return o, usageError("--fault must be none|after-normalize|before-commit")
	}
	return o, nil
}
