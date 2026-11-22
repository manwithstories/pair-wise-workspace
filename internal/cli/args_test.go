package cli_test

import (
	"testing"

	"mediarun/internal/cli"
	"mediarun/internal/mediaerr"
)

func TestParseDefaults(t *testing.T) {
	a, err := cli.Parse([]string{"demo", "--work", "./tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkDir != "./tmp/x" || a.Format != "mixed" {
		t.Fatalf("defaults wrong: %+v", a)
	}
	if a.QueueDepth != 8 || a.TargetBitrate != 12_000_000 {
		t.Fatalf("default depth/bitrate wrong: %+v", a)
	}
	if a.Resume {
		t.Fatal("resume must default to false")
	}
}

func TestParseEqualsAndBooleanFlags(t *testing.T) {
	a, err := cli.Parse([]string{
		"demo", "--work=./w", "--queue-depth=4", "--resume",
		"--interrupt-after", "97", "--format", "mxf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.QueueDepth != 4 || !a.Resume || a.InterruptAfter != 97 || a.Format != "mxf" {
		t.Fatalf("parsed wrong: %+v", a)
	}
}

func TestParseRejectsBadArgs(t *testing.T) {
	cases := [][]string{
		{},
		{"frobnicate"},
		{"demo"},
		{"demo", "--work", "./w", "--format", "wav"},
		{"demo", "--work", "./w", "--queue-depth", "0"},
		{"demo", "--work", "./w", "--target-bitrate", "-5"},
		{"demo", "--work", "./w", "--bogus", "1"},
		{"demo", "--work", "./w", "--resume=true"},
		{"demo", "--work", "./w", "--inject", "explode"},
	}
	for i, argv := range cases {
		_, err := cli.Parse(argv)
		if !mediaerr.Is(err, mediaerr.CodeInvalidArgs) {
			t.Errorf("case %d %v: want INVALID_ARGS, got %v", i, argv, err)
		}
		if cli.ExitCode(err) != 1 {
			t.Errorf("case %d: exit code = %d, want 1", i, cli.ExitCode(err))
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	checks := map[mediaerr.ErrorCode]int{
		mediaerr.CodeInvalidArgs:       1,
		mediaerr.CodeInvalidConfig:     1,
		mediaerr.CodePathTraversal:     1,
		mediaerr.CodeTruncatedBox:      2,
		mediaerr.CodeTruncatedKLV:      2,
		mediaerr.CodeCropOutOfBounds:   2,
		mediaerr.CodeCheckpointMismatch: 3,
	}
	for code, want := range checks {
		if got := cli.ExitCode(mediaerr.New(code, "x")); got != want {
			t.Errorf("%s -> %d, want %d", code, got, want)
		}
	}
	if cli.ExitCode(nil) != 0 {
		t.Error("nil error must map to exit 0")
	}
}

func TestMappingTargetsBothTracks(t *testing.T) {
	a := cli.Args{}
	cfg := a.Mapping()
	if len(cfg.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(cfg.Rules))
	}
}
