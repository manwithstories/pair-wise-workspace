package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"relayctl/internal/peer"
	"relayctl/internal/relay"
	"relayctl/internal/signal"
	"strings"
)

const (
	exitOK        = 0
	exitRuntime   = 1
	exitInput     = 2
	exitThreshold = 3
	exitPolicy    = 4
)

type codedError struct {
	code string
	hint string
	err  error
}

func (e *codedError) Error() string { return e.code + ": " + e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func withCode(code, hint string, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, hint: hint, err: err}
}

type thresholdError struct {
	failures []string
}

func (e *thresholdError) Error() string { return strings.Join(e.failures, "; ") }

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		code, errorCode, hint := classifyError(err)
		fmt.Fprintf(os.Stderr, "error_code=%s message=%q hint=%q\n", errorCode, err.Error(), hint)
		os.Exit(code)
	}
}

func runCLI(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return withCode("bad_args", "use probe, demo, verify or sink", errors.New("missing subcommand"))
	}
	switch args[0] {
	case "probe":
		return runProbe(args[1:], stdout)
	case "demo":
		return runDemo(args[1:], stdout)
	case "verify":
		return runVerify(args[1:], stdout)
	case "sink":
		return runSink(args[1:], stdout)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return withCode("bad_args", "use probe, demo, verify or sink", fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "relayctl probe  --bind 127.0.0.1:0 --want-pairs 2 --json out/pairs.json")
	fmt.Fprintln(w, "relayctl demo   --packets 3000 --duration 6s --seed 42 --impair loss=0.01,dup=0.01,reorder=0.02 --sink 127.0.0.1:0 --json out/report.json")
	fmt.Fprintln(w, "relayctl verify --report out/report.json --min-forward-ratio 0.98 --max-jitter-p95-ms 30")
	fmt.Fprintln(w, "relayctl sink   --bind 127.0.0.1:5004 --duration 10s --json out/sink.json")
}

func classifyError(err error) (int, string, string) {
	var threshold *thresholdError
	if errors.As(err, &threshold) {
		return exitThreshold, "thresholds_failed", "adjust thresholds or inspect the report metrics"
	}
	if errors.Is(err, relay.ErrNonLoopbackTarget) || errors.Is(err, signal.ErrNonLoopbackCandidate) || errors.Is(err, peer.ErrNonLoopbackCandidate) {
		return exitPolicy, "loopback_policy_violation", "bind and send only to 127.0.0.1 or ::1"
	}
	if errors.Is(err, relay.ErrInvalidReport) {
		return exitInput, "invalid_report", "provide a relayctl JSON report"
	}
	var coded *codedError
	if errors.As(err, &coded) {
		code := exitRuntime
		if coded.code == "invalid_report" {
			code = exitInput
		}
		return code, coded.code, coded.hint
	}
	if errors.Is(err, signal.ErrSignalTimeout) {
		return exitRuntime, "signal_timeout", "check the signalling bind address and that both roles start"
	}
	if errors.Is(err, peer.ErrICEFailed) {
		return exitRuntime, "ice_failed", "verify loopback candidate exchange and interface filtering"
	}
	if errors.Is(err, peer.ErrConnectionTimeout) {
		return exitRuntime, "ice_timeout", "verify signalling candidates and firewall-free loopback"
	}
	if isAddressInUse(err) {
		return exitRuntime, "port_in_use", "choose another --bind or stop the existing listener"
	}
	return exitRuntime, "runtime_error", "inspect the message and rerun with a loopback bind"
}

func isAddressInUse(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "address already in use") || strings.Contains(text, "bind: address already in use")
}

func writeJSON(path string, value any) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create JSON directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace JSON: %w", err)
	}
	return nil
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return withCode("bad_args", "use a loopback host:port", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: %s", signal.ErrNonLoopbackCandidate, addr)
	}
	return nil
}

func signalWireError(m *signal.Message) error {
	if m == nil || m.Type != signal.TypeError {
		return nil
	}
	err := fmt.Errorf("signal %s: %s", m.Code, m.Message)
	switch m.Code {
	case signal.CodeCandidateBanned:
		return fmt.Errorf("%w: %s", signal.ErrNonLoopbackCandidate, err)
	case signal.CodeDuplicateOffer:
		return withCode("duplicate_offer", "a session may negotiate only one offer", err)
	case signal.CodeCandidateMalformed, signal.CodeInvalidMessage:
		return withCode("sdp_or_candidate_invalid", "check SDP and ICE candidate fields", err)
	default:
		return withCode(string(m.Code), "inspect signalling role and phase", err)
	}
}
