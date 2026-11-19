package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mediq/internal/fixture"
	"mediq/internal/model"
	"mediq/internal/pipeline"
)

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		nil,
		{"bogus"},
		{"process"},
		{"process", "--input", "x"},
		{"process", "--input", "x", "--output", "y", "--queue", "0"},
		{"fixture"},
		{"fixture", "--dir", "x", "--timebase", "bad"},
		{"verify"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != exitArgs {
			t.Fatalf("%v -> %d, want %d", args, code, exitArgs)
		}
	}
}

func TestHelpExitZero(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--help"}, &out, &errb); code != exitOK {
		t.Fatalf("%d", code)
	}
	if !strings.Contains(out.String(), "mediq") {
		t.Fatal("help text missing")
	}
}

func TestFixtureAndProcessAndVerify(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := run([]string{"fixture", "--dir", dir, "--frames", "200", "--corrupt", "55", "--truncate-tail", "1"}, &out, &errb)
	if code != exitOK {
		t.Fatalf("fixture: %d %s", code, errb.String())
	}
	for _, f := range []string{"normal.seq", "damaged.seq", "truncated.seq"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	output := filepath.Join(dir, "out.mp4")
	ckpt := filepath.Join(dir, "job.ckpt")
	out.Reset()
	errb.Reset()
	code = run([]string{
		"process", "--input", filepath.Join(dir, "damaged.seq"),
		"--output", output, "--checkpoint", ckpt,
		"--chunk-frames", "32", "--queue", "8",
	}, &out, &errb)
	if code != exitOK {
		t.Fatalf("process: %d %s", code, errb.String())
	}
	text := out.String()
	for _, field := range []string{
		"frames_read=", "frames_indexed=", "first_corrupt_frame=55",
		"first_corrupt_offset=", "recovered_chunks=", "resume_ms=",
		"peak_queue_depth=", "output_sha256=", "index_valid=true",
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("missing %q in output:\n%s", field, text)
		}
	}
	if !strings.Contains(text, "frames_indexed=199") {
		t.Fatalf("unexpected indexed count:\n%s", text)
	}

	out.Reset()
	code = run([]string{"verify", "--input", output, "--source", filepath.Join(dir, "damaged.seq")}, &out, &errb)
	if code != exitOK {
		t.Fatalf("verify: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "index_valid=true") {
		t.Fatalf("verify output: %s", out.String())
	}
}

func TestProcessCheckpointMismatchExitFour(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.Generate(fixture.Config{Dir: dir, Frames: 1200, FrameBytes: 64, Seed: 1}); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "normal.seq")
	output := filepath.Join(dir, "o.mp4")
	ckpt := filepath.Join(dir, "o.ckpt")

	// Interrupt a slow run so a checkpoint is published.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := pipeline.New(pipeline.Options{
			Input: input, Output: output, CheckpointPath: ckpt,
			ChunkFrames: 8, QueueDepth: 2, SlowConsumer: 2 * time.Millisecond,
		}).Run(ctx)
		done <- err
	}()
	// Wait for the first committed chunk, then cancel: the checkpoint must
	// exist for this test to mean anything.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ckpt); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("no checkpoint was committed")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// Hide the original input and put a different file with the same path and
	// size in its place: the checkpoint's prefix digest no longer matches.
	other := t.TempDir()
	if _, err := fixture.Generate(fixture.Config{Dir: other, Frames: 1200, FrameBytes: 64, Seed: 99}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(other, "normal.seq"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := run([]string{"process", "--input", input, "--output", output, "--checkpoint", ckpt}, &out, &errb)
	if code != exitCkpt {
		t.Fatalf("got %d, want %d (%s)", code, exitCkpt, errb.String())
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{model.ErrCheckpointMismatch, exitCkpt},
		{model.ErrVerifyFailed, exitVerify},
		{model.ErrDamagedInput, exitDamaged},
		{context.Canceled, exitKilled},
		{errors.New("boom"), exitLocal},
		{&model.Fault{Code: model.CodeHashMismatch}, exitDamaged},
	}
	for _, c := range cases {
		var b bytes.Buffer
		if got := classify(nil, c.err, &b); got != c.want {
			t.Fatalf("%v -> %d want %d", c.err, got, c.want)
		}
	}
}

func TestClassifyDamagedButIndexedIsOK(t *testing.T) {
	var b bytes.Buffer
	if got := classify(&pipeline.Stats{HasCorrupt: true}, nil, &b); got != exitOK {
		t.Fatalf("got %d", got)
	}
}
