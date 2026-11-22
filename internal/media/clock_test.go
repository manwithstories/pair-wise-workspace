package media

import (
	"encoding/json"
	"errors"
	"math/rand"
	"testing"
)

func mustTB(t *testing.T, ts int64) TimeBase {
	t.Helper()
	tb, err := NewTimeBase(ts)
	if err != nil {
		t.Fatalf("time base %d: %v", ts, err)
	}
	return tb
}

// TestExactConversion checks the rational mapping against a rational oracle.
func TestExactConversion(t *testing.T) {
	src := mustTB(t, 44100)
	dst := mustTB(t, 48000)
	for _, ticks := range []int64{0, 1, 147, 441, 4410, 44100, 123456789} {
		got, rem, err := Exact(ticks, src, dst)
		if err != nil {
			t.Fatalf("exact(%d): %v", ticks, err)
		}
		if want := ticks * 48000 / 44100; got != want {
			t.Fatalf("exact(%d)=%d, want %d", ticks, got, want)
		}
		if rem < 0 || rem >= src.Timescale {
			t.Fatalf("remainder %d out of range", rem)
		}
	}
}

// TestDriftBounded proves the accumulated conversion error of a whole
// stream never reaches one destination tick.
func TestDriftBounded(t *testing.T) {
	src := mustTB(t, 44100)
	dst := mustTB(t, 48000)
	d, err := NewDrift(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		n := int64(rng.Intn(1 << 20))
		got, err := d.At(n)
		if err != nil {
			t.Fatal(err)
		}
		exactNum := n * dst.Timescale
		diff := got*src.Timescale - exactNum
		// |got*src - n*dst| < src means the time error is under one
		// destination tick after scaling.
		if diff < 0 {
			diff = -diff
		}
		if diff >= src.Timescale {
			t.Fatalf("n=%d: drift %d ticks >= 1 destination tick", n, diff)
		}
	}
}

// TestRoundHalfUp checks tie-breaking is deterministic.
func TestRoundHalfUp(t *testing.T) {
	src := mustTB(t, 2)
	dst := mustTB(t, 4)
	got, err := Round(1, src, dst) // 1/2 s = 2 ticks, exact
	if err != nil || got != 2 {
		t.Fatalf("round(1)=%d, %v", got, err)
	}
	got, _ = Round(1, mustTB(t, 4), mustTB(t, 2)) // 0.25 s -> 0.5 ticks -> 1
	if got != 1 {
		t.Fatalf("half tie rounded to %d, want 1", got)
	}
}

// TestInvalidTimeBase rejects zero and negative timescales.
func TestInvalidTimeBase(t *testing.T) {
	for _, ts := range []int64{0, -1, -48000} {
		if _, err := NewTimeBase(ts); err == nil {
			t.Fatalf("timescale %d accepted", ts)
		}
	}
	if _, _, err := Exact(1, TimeBase{}, mustTB(t, 48000)); err == nil {
		t.Fatal("zero source timescale accepted")
	}
}

// TestLCM checks the shared exact intermediate base.
func TestLCM(t *testing.T) {
	got, err := LCM(mustTB(t, 44100), mustTB(t, 48000))
	if err != nil {
		t.Fatal(err)
	}
	if got%44100 != 0 || got%48000 != 0 {
		t.Fatalf("lcm %d is not a common multiple", got)
	}
}

// TestErrorSerialisation checks the machine-readable shape and exit mapping.
func TestErrorSerialisation(t *testing.T) {
	e := NewError(CodeTruncatedMdat, 177083, 1, "mdat truncated")
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back["error_code"] != "TRUNCATED_MDAT" {
		t.Fatalf("code = %v", back["error_code"])
	}
	if back["offset"].(float64) != 177083 {
		t.Fatalf("offset = %v", back["offset"])
	}
	if got := e.Line(); got != "error_code=TRUNCATED_MDAT offset=177083 track=1" {
		t.Fatalf("line = %q", got)
	}
	if e.ExitCode() != ExitProcessing {
		t.Fatalf("exit = %d", e.ExitCode())
	}
}

// TestOffsetZeroIsDistinguished proves a genuine offset 0 is not confused
// with "unknown position".
func TestOffsetZeroIsDistinguished(t *testing.T) {
	zero := NewError(CodeInvalidBoxLength, 0, 0, "at file start")
	if !zero.HasOffset {
		t.Fatal("offset 0 lost")
	}
	unknown := NewError(CodeUsage, -1, 0, "no position")
	if unknown.HasOffset {
		t.Fatal("unknown position marked as offset 0")
	}
	if got := unknown.Line(); got != "error_code=USAGE" {
		t.Fatalf("line = %q", got)
	}
}

// TestExitCodes pins the contract used by the acceptance script.
func TestExitCodes(t *testing.T) {
	cases := map[Code]int{
		CodeUsage:              ExitInput,
		CodeUnsupportedCodec:   ExitInput,
		CodeCheckpointConflict: ExitCheckpoint,
		CodeTruncatedMdat:      ExitProcessing,
		CodeOutputVerification: ExitProcessing,
	}
	for code, want := range cases {
		if got := NewError(code, 0, 0, "").ExitCode(); got != want {
			t.Errorf("%s: exit %d, want %d", code, got, want)
		}
	}
	if ExitCodeOf(nil) != ExitOK {
		t.Error("nil error should exit 0")
	}
	if ExitCodeOf(errors.New("plain")) != ExitProcessing {
		t.Error("plain error should exit 4")
	}
}

// TestErrorChain checks errors.As finds a wrapped typed error.
func TestErrorChain(t *testing.T) {
	inner := NewError(CodeSampleBeyondMdat, 42, 2, "sample outside mdat")
	wrapped := errors.Join(errors.New("context"), inner)
	got, ok := AsError(wrapped)
	if !ok || got.Code != CodeSampleBeyondMdat || got.Track != 2 {
		t.Fatalf("got %+v", got)
	}
}
