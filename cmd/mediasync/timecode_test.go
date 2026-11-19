package main

import (
	"strings"
	"testing"

	"mediasync/internal/testdata"
)

func TestTimecodeCommand(t *testing.T) {
	fx := testdata.TimecodeFixtures()[0]
	path := writeTemp(t, "tc.txt", testdata.TimecodeList(fx))
	code, out, _ := invoke(t, "timecode", path, "--frames", fx.Rate)
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "rollovers=1") || !strings.Contains(out, "duplicates=1") {
		t.Errorf("output = %s", out)
	}
}

func TestTimecodeRejectsBrokenLabels(t *testing.T) {
	fx := testdata.TimecodeFixtures()[3]
	data := testdata.TimecodeList(fx)
	data = append(data, []byte(strings.Join(fx.Broken, "\n")+"\n")...)
	path := writeTemp(t, "tc.txt", data)
	code, out, _ := invoke(t, "timecode", path, "--frames", fx.Rate)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if strings.Count(out, "recover ") != len(fx.Broken) {
		t.Errorf("reported %d recoverable issues, want %d:\n%s", strings.Count(out, "recover "), len(fx.Broken), out)
	}
}

func TestTimecodeArgumentErrors(t *testing.T) {
	if code, _, _ := invoke(t, "timecode"); code != 1 {
		t.Error("missing args accepted")
	}
	path := writeTemp(t, "tc.txt", []byte("00:00:00:00\n"))
	if code, _, _ := invoke(t, "timecode", path, "--frames", "oops"); code != 1 {
		t.Error("bad frame rate accepted")
	}
	if code, _, _ := invoke(t, "timecode", path); code != 1 {
		t.Error("missing --frames accepted")
	}
	if code, _, _ := invoke(t, "timecode", path, "--frames", "25", "extra"); code != 1 {
		t.Error("extra argument accepted")
	}
}
