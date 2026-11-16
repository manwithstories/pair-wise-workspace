package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func parse(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	return Parse(args, &bytes.Buffer{})
}

func TestParseAcceptsDemoFlags(t *testing.T) {
	opts, err := parse(t, "demo", "--packets", "2000", "--interval-ms", "1",
		"--reorder", "32", "--duplicate-rate", "0.02", "--loss-rate", "0.01", "--json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Command != "demo" || opts.Packets != 2000 || opts.IntervalMs != 1 {
		t.Fatalf("flags not applied: %+v", opts)
	}
	if opts.Reorder != 32 || opts.DuplicateRate != 0.02 || opts.LossRate != 0.01 {
		t.Fatalf("fault flags not applied: %+v", opts)
	}
	if !opts.JSON {
		t.Fatal("--json not honoured")
	}
	cfg := opts.FixtureConfig()
	if cfg.Packets != 2000 || cfg.ReorderWindow != 32 || cfg.LossRate != 0.01 {
		t.Fatalf("fixture config derived incorrectly: %+v", cfg)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"demo", "--packets", "0"},
		{"demo", "--packets", "-1"},
		{"demo", "--interval-ms", "0"},
		{"demo", "--reorder", "-1"},
		{"demo", "--reorder", "9999"},
		{"demo", "--queue", "0"},
		{"demo", "--loss-rate", "1.1"},
		{"demo", "--loss-rate", "-0.1"},
		{"demo", "--duplicate-rate", "2"},
		{"demo", "--reorder-rate", "-1"},
		{"demo", "--connect-ms", "0"},
		{"demo", "--media-ms", "-5"},
		{"demo", "--idle-ms", "0"},
		{"demo", "--json", "extra"},
	}
	for _, args := range cases {
		if _, err := parse(t, args...); err == nil {
			t.Fatalf("args %v should have been rejected", args)
		} else if !errors.Is(err, ErrUsage) {
			t.Fatalf("args %v: want ErrUsage, got %v", args, err)
		}
	}
}

func TestParseAcceptsAllSubcommands(t *testing.T) {
	for _, cmd := range []string{"demo", "serve", "probe"} {
		if _, err := parse(t, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
}

func TestProbeAddrOverridesAddr(t *testing.T) {
	opts, err := parse(t, "probe", "--addr", "127.0.0.1:1", "--probe-addr", "127.0.0.1:2")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Addr != "127.0.0.1:2" {
		t.Fatalf("--probe-addr did not win: %s", opts.Addr)
	}
}

func TestUsageMentionsEveryExitCode(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	for _, want := range []string{"demo", "serve", "probe", "2", "3", "4"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("usage text is missing %q:\n%s", want, buf.String())
		}
	}
}

func TestTimeoutsAreDerivedFromMillis(t *testing.T) {
	opts, err := parse(t, "demo", "--connect-ms", "1234", "--media-ms", "567", "--drain-ms", "42", "--idle-ms", "999")
	if err != nil {
		t.Fatal(err)
	}
	if opts.ConnectTimeout().Milliseconds() != 1234 {
		t.Fatalf("connect timeout: %v", opts.ConnectTimeout())
	}
	if opts.MediaTimeout().Milliseconds() != 567 {
		t.Fatalf("media timeout: %v", opts.MediaTimeout())
	}
	if opts.Drain().Milliseconds() != 42 {
		t.Fatalf("drain: %v", opts.Drain())
	}
	if opts.Idle().Milliseconds() != 999 {
		t.Fatalf("idle: %v", opts.Idle())
	}
}
