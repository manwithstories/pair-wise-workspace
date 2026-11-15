package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loudness/internal/gen"
)

func TestCLIExitCodesAndOutputs(t *testing.T) {
	dir := t.TempDir()
	if _, err := gen.WriteSuite(dir); err != nil {
		t.Fatal(err)
	}

	t.Run("measure pass json deterministic", func(t *testing.T) {
		args := []string{"measure", "--in", filepath.Join(dir, "sine_1k_stereo_48k.wav"), "--json"}
		var first, second, stderr bytes.Buffer
		if code := run(args, &first, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if code := run(args, &second, &stderr); code != 0 {
			t.Fatalf("second exit=%d", code)
		}
		if first.String() != second.String() {
			t.Fatalf("json differs:\n%s\n%s", first.String(), second.String())
		}
		if !strings.Contains(first.String(), `"gated_blocks":117`) || !strings.Contains(first.String(), `"verdict":"PASS"`) {
			t.Fatalf("json = %s", first.String())
		}
		if strings.Count(first.String(), "\n") != 1 {
			t.Fatalf("json is not one line: %q", first.String())
		}
	})

	t.Run("quiet lead in fails below target", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"measure", "--in", filepath.Join(dir, "quiet_lead_in.wav")}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "verdict=FAIL\n") || !strings.Contains(stdout.String(), "reason=below_target\n") {
			t.Fatalf("stdout = %s", stdout.String())
		}
	})

	t.Run("batch summary and exit 2", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"batch", "--dir", dir, "--jobs", "4"}, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "summary total=8 pass=5 fail=2 error=1\n") {
			t.Fatalf("stdout = %s", stdout.String())
		}
	})

	t.Run("output file", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "result.json")
		var stdout, stderr bytes.Buffer
		code := run([]string{"measure", "--in", filepath.Join(dir, "sine_1k_stereo_48k.wav"), "--json", "-o", out}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q", stdout.String())
		}
		b, err := os.ReadFile(out)
		if err != nil || !strings.Contains(string(b), `"verdict":"PASS"`) {
			t.Fatalf("output file: %v %s", err, b)
		}
	})
}

func TestCLIUsageAndDecodeErrors(t *testing.T) {
	if code := run(nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 3 {
		t.Fatalf("no args exit = %d", code)
	}
	if code := run([]string{"measure"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 3 {
		t.Fatalf("missing input exit = %d", code)
	}
	path := filepath.Join(t.TempDir(), "bad-rate.wav")
	if err := gen.WriteWAV(path, gen.WAVSpec{SampleRate: 22050, Channels: 1, Bits: 16, Seconds: 1},
		func(frame, ch int) float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"measure", "--in", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("unsupported rate exit = %d stderr=%s", code, stderr.String())
	}
	layoutPath := filepath.Join(t.TempDir(), "bad-layout.wav")
	if err := gen.WriteWAV(layoutPath, gen.WAVSpec{SampleRate: 48000, Channels: 6, Bits: 16,
		Extensible: true, ChannelMask: 0x3e, Seconds: 1}, func(frame, ch int) float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"measure", "--in", layoutPath}, &stdout, &stderr); code != 2 {
		t.Fatalf("unsupported layout exit = %d stderr=%s", code, stderr.String())
	}
}

func TestSelftestPrintsSixLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"selftest"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("lines=%d output=%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "expected=") || !strings.Contains(line, "actual=") || !strings.Contains(line, "err=") {
			t.Fatalf("bad selftest line: %s", line)
		}
	}
}
